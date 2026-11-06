// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Tests for the machine-principal token source. They mirror the controller's
//! `beadsapi/machine_token_test.go` case for case: env gating, fetch + cache +
//! half-life refresh, fail-closed on an unreachable endpoint or a missing
//! secret, cached-until-expiry on a refresh failure, secret re-read, and the
//! unverified claims decode used for the first-fetch log line.

use std::sync::Arc;
use std::time::{Duration, Instant};

use base64::Engine as _;
use serial_test::serial;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

use super::*;

/// A pinned, manually advanced clock.
type TestClock = Arc<std::sync::Mutex<Instant>>;

fn test_clock() -> (TestClock, Clock) {
    let now: TestClock = Arc::new(std::sync::Mutex::new(Instant::now()));
    let handle = Arc::clone(&now);
    (now, Box::new(move || *handle.lock().unwrap()))
}

fn advance(clock: &TestClock, by: Duration) {
    *clock.lock().unwrap() += by;
}

/// Unsigned JWT with the given payload; only the payload is ever read.
fn jwt(payload: serde_json::Value) -> String {
    let p = base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(payload.to_string());
    format!("eyJhbGciOiJub25lIn0.{p}.sig")
}

fn token_body(token: &str, expires_in: Option<u64>) -> String {
    let mut v = serde_json::json!({ "access_token": token, "token_type": "Bearer" });
    if let Some(e) = expires_in {
        v["expires_in"] = serde_json::json!(e);
    }
    v.to_string()
}

fn secret_file(dir: &tempfile::TempDir, secret: &str) -> std::path::PathBuf {
    let p = dir.path().join("clientSecret");
    std::fs::write(&p, format!("{secret}\n")).unwrap();
    p
}

fn find(hay: &[u8], needle: &[u8]) -> Option<usize> {
    hay.windows(needle.len()).position(|w| w == needle)
}

/// Read one HTTP/1.1 request (headers + Content-Length body) off the stream.
async fn read_request(stream: &mut TcpStream) -> String {
    let mut buf = Vec::new();
    let mut chunk = [0u8; 4096];
    loop {
        let Ok(n) = stream.read(&mut chunk).await else { break };
        if n == 0 {
            break;
        }
        buf.extend_from_slice(&chunk[..n]);
        if let Some(pos) = find(&buf, b"\r\n\r\n") {
            let head = String::from_utf8_lossy(&buf[..pos]).to_string();
            let content_length = head
                .lines()
                .find_map(|l| {
                    let (k, v) = l.split_once(':')?;
                    k.eq_ignore_ascii_case("content-length")
                        .then(|| v.trim().parse::<usize>().ok())?
                })
                .unwrap_or(0);
            if buf.len() >= pos + 4 + content_length {
                break;
            }
        }
    }
    String::from_utf8_lossy(&buf).to_string()
}

/// A token endpoint that answers `responses` in order, one connection each,
/// and records every raw request it saw.
async fn token_server(
    responses: Vec<(u16, String)>,
) -> anyhow::Result<(String, Arc<tokio::sync::Mutex<Vec<String>>>)> {
    let listener = TcpListener::bind("127.0.0.1:0").await?;
    let url = format!("http://{}/token", listener.local_addr()?);
    let captured = Arc::new(tokio::sync::Mutex::new(Vec::new()));
    let cap = Arc::clone(&captured);
    tokio::spawn(async move {
        for (status, body) in responses {
            let Ok((mut stream, _)) = listener.accept().await else { return };
            let req = read_request(&mut stream).await;
            cap.lock().await.push(req);
            let reason = if status == 200 { "OK" } else { "Error" };
            let resp = format!(
                "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                body.len()
            );
            let _ = stream.write_all(resp.as_bytes()).await;
            let _ = stream.shutdown().await;
        }
    });
    Ok((url, captured))
}

/// A URL nothing listens on.
async fn closed_port_url() -> anyhow::Result<String> {
    let l = TcpListener::bind("127.0.0.1:0").await?;
    let url = format!("http://{}/token", l.local_addr()?);
    drop(l);
    Ok(url)
}

fn clear_env() {
    for k in [ENV_TOKEN_URL, ENV_CLIENT_ID, ENV_CLIENT_SECRET_FILE, ENV_CA_FILE] {
        std::env::remove_var(k);
    }
}

// --- env gating -------------------------------------------------------------

#[test]
#[serial]
fn from_env_none_set_is_disabled() -> anyhow::Result<()> {
    clear_env();
    assert!(MachineTokenSource::from_env()?.is_none());
    Ok(())
}

#[test]
#[serial]
fn from_env_partial_env_is_an_error() -> anyhow::Result<()> {
    clear_env();
    std::env::set_var(ENV_TOKEN_URL, "http://keycloak.svc:8080/token");
    let err = match MachineTokenSource::from_env() {
        Ok(_) => {
            clear_env();
            anyhow::bail!("partial env must be an error");
        }
        Err(e) => e,
    };
    assert!(err.to_string().contains("must all be set"), "got: {err}");

    std::env::set_var(ENV_CLIENT_ID, "gardenio-coopmux");
    assert!(MachineTokenSource::from_env().is_err(), "two of three is still partial");
    clear_env();
    Ok(())
}

#[test]
#[serial]
fn from_env_all_set_is_enabled() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    clear_env();
    std::env::set_var(ENV_TOKEN_URL, " http://keycloak.svc:8080/token ");
    std::env::set_var(ENV_CLIENT_ID, "gardenio-coopmux");
    std::env::set_var(ENV_CLIENT_SECRET_FILE, "/var/run/secrets/gardenio-machine/clientSecret");
    let src = MachineTokenSource::from_env()?;
    clear_env();
    let src = src.ok_or_else(|| anyhow::anyhow!("all three set must enable"))?;
    assert_eq!(src.client_id(), "gardenio-coopmux");
    assert_eq!(src.token_url, "http://keycloak.svc:8080/token", "values are trimmed");
    Ok(())
}

#[test]
#[serial]
fn from_env_reads_the_optional_ca_file() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    clear_env();
    std::env::set_var(ENV_TOKEN_URL, "https://keycloak.svc:8443/token");
    std::env::set_var(ENV_CLIENT_ID, "gardenio-coopmux");
    std::env::set_var(ENV_CLIENT_SECRET_FILE, "/var/run/secrets/gardenio-machine/clientSecret");
    let without = MachineTokenSource::from_env()?;
    std::env::set_var(ENV_CA_FILE, " /etc/gardenio/keycloak-runtime-ca/ca.crt ");
    let with = MachineTokenSource::from_env()?;
    clear_env();
    let without = without.ok_or_else(|| anyhow::anyhow!("all three set must enable"))?;
    let with = with.ok_or_else(|| anyhow::anyhow!("all three set must enable"))?;
    assert_eq!(without.ca_file, None);
    assert_eq!(
        with.ca_file.as_deref(),
        Some(std::path::Path::new("/etc/gardenio/keycloak-runtime-ca/ca.crt")),
        "the CA path is trimmed and does not count toward the three"
    );
    Ok(())
}

// --- in-cluster CA (gd-lYPZlslOM7) ---------------------------------------------

/// A self-signed P-256 certificate (public half only), standing in for the
/// keycloak-runtime CA.
const TEST_CA_PEM: &str = "-----BEGIN CERTIFICATE-----
MIIBizCCATGgAwIBAgIUIERKscN+5LJB5ksvkTUKYsfIzjwwCgYIKoZIzj0EAwIw
GjEYMBYGA1UEAwwPY29vcG11eCB0ZXN0IENBMCAXDTI2MTAyNTE4MzgxOVoYDzIx
MjYxMDAxMTgzODE5WjAaMRgwFgYDVQQDDA9jb29wbXV4IHRlc3QgQ0EwWTATBgcq
hkjOPQIBBggqhkjOPQMBBwNCAATr5255zJR6nVdYoaLszZHu0s6nVJVNqmQKMnD0
9QzbNsTo/DxqVz6PAdkQFLuZYLD7LrFwjKAgsxQOJ9bmnZNio1MwUTAdBgNVHQ4E
FgQUYzklPgXHZ6AClvugUMtCtXYOR10wHwYDVR0jBBgwFoAUYzklPgXHZ6AClvug
UMtCtXYOR10wDwYDVR0TAQH/BAUwAwEB/zAKBggqhkjOPQQDAgNIADBFAiEA+jQ+
B+mcmjjr69c50JDChmFqF0ZcD/HtKDHJQhMkFNACIHAda6Ex6XcTEVxxMKHTu9aw
+UDUo/P7rV9agdXzvXAz
-----END CERTIFICATE-----
";

#[tokio::test]
async fn ca_file_missing_or_not_pem_is_auth_unavailable() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "s3cr3t");
    // Zero responses: an exchange without a usable CA would hang, not pass.
    let (url, captured) = token_server(vec![]).await?;
    let ca = dir.path().join("ca.crt");
    let src = MachineTokenSource::new(url, "c".into(), &secret)?.with_ca_file(&ca);

    let err = match src.token().await {
        Ok(_) => anyhow::bail!("a missing CA file must fail"),
        Err(e) => e.to_string(),
    };
    assert!(
        err.contains(AUTH_UNAVAILABLE) && err.contains("CA") && err.contains("unreadable"),
        "got: {err}"
    );

    std::fs::write(&ca, "not a certificate\n")?;
    let err = match src.token().await {
        Ok(_) => anyhow::bail!("a non-PEM CA file must fail"),
        Err(e) => e.to_string(),
    };
    assert!(err.contains(AUTH_UNAVAILABLE) && err.contains("no PEM certificate"), "got: {err}");
    assert!(captured.lock().await.is_empty(), "no exchange without a usable CA");
    Ok(())
}

#[tokio::test]
async fn ca_file_appearing_later_recovers_without_a_new_source() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "s3cr3t");
    let t1 = jwt(serde_json::json!({ "azp": "c" }));
    let (url, captured) = token_server(vec![(200, token_body(&t1, Some(1800)))]).await?;
    let ca = dir.path().join("ca.crt");
    let src = MachineTokenSource::new(url, "c".into(), &secret)?.with_ca_file(&ca);

    assert!(src.token().await.is_err(), "fails closed before the CA exists");
    // kubelet projects the CA once cert-manager has issued.
    std::fs::write(&ca, TEST_CA_PEM)?;
    assert_eq!(src.token().await?, t1);
    assert_eq!(captured.lock().await.len(), 1);
    Ok(())
}

// --- fetch, cache, refresh ----------------------------------------------------

#[tokio::test]
async fn fetch_cache_and_half_life_refresh() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "s3cr3t");
    let t1 = jwt(serde_json::json!({
        "azp": "gardenio-coopmux",
        "preferred_username": "service-account-gardenio-coopmux",
        "aud": "gardenio-traefik-forwardauth",
        "realm_roles": ["gardenio-machine-accounts", "gardenio-operator"],
        "exp": 1_800_000, "iat": 1_798_200
    }));
    let t2 = jwt(serde_json::json!({ "azp": "gardenio-coopmux" }));
    let (url, captured) =
        token_server(vec![(200, token_body(&t1, Some(1800))), (200, token_body(&t2, Some(1800)))])
            .await?;
    let (clock, clock_fn) = test_clock();
    let src =
        MachineTokenSource::new(url, "gardenio-coopmux".into(), &secret)?.with_clock(clock_fn);

    assert_eq!(src.token().await?, t1);
    assert_eq!(src.token().await?, t1, "second call is served from cache");
    assert_eq!(captured.lock().await.len(), 1, "one fetch so far");

    // The exchange is a client-credentials grant with the secret in a Basic
    // header (AuthStyleInHeader), never in the body or the URL.
    let req = captured.lock().await[0].to_lowercase();
    assert!(req.starts_with("post /token "), "got: {req}");
    let basic = base64::engine::general_purpose::STANDARD.encode("gardenio-coopmux:s3cr3t");
    assert!(req.contains(&format!("authorization: basic {}", basic.to_lowercase())), "got: {req}");
    assert!(req.contains("content-type: application/x-www-form-urlencoded"), "got: {req}");
    assert!(req.ends_with("grant_type=client_credentials"), "got: {req}");
    assert!(!req.contains("s3cr3t"), "secret must not appear in clear: {req}");

    // Just before half-life: still cached. Just after: refreshed.
    advance(&clock, Duration::from_secs(899));
    assert_eq!(src.token().await?, t1);
    assert_eq!(captured.lock().await.len(), 1);
    advance(&clock, Duration::from_secs(2));
    assert_eq!(src.token().await?, t2);
    assert_eq!(captured.lock().await.len(), 2);
    Ok(())
}

#[tokio::test]
async fn lifetime_falls_back_to_jwt_then_default() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "s");

    // No expires_in: exp-iat (600s) drives the half-life (300s).
    let t1 = jwt(serde_json::json!({ "exp": 1_000_600, "iat": 1_000_000 }));
    // Neither: the 5-minute default (half-life 150s).
    let t2 = "opaque.token".to_owned();
    let t3 = jwt(serde_json::json!({ "azp": "x" }));
    let (url, captured) = token_server(vec![
        (200, token_body(&t1, None)),
        (200, token_body(&t2, None)),
        (200, token_body(&t3, None)),
    ])
    .await?;
    let (clock, clock_fn) = test_clock();
    let src = MachineTokenSource::new(url, "c".into(), &secret)?.with_clock(clock_fn);

    assert_eq!(src.token().await?, t1);
    advance(&clock, Duration::from_secs(299));
    assert_eq!(src.token().await?, t1);
    advance(&clock, Duration::from_secs(2));
    assert_eq!(src.token().await?, t2);
    advance(&clock, Duration::from_secs(149));
    assert_eq!(src.token().await?, t2);
    advance(&clock, Duration::from_secs(2));
    assert_eq!(src.token().await?, t3);
    assert_eq!(captured.lock().await.len(), 3);
    Ok(())
}

#[tokio::test]
async fn secret_file_is_reread_on_refresh() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "old");
    let (url, captured) =
        token_server(vec![(200, token_body("t1", Some(100))), (200, token_body("t2", Some(100)))])
            .await?;
    let (clock, clock_fn) = test_clock();
    let src = MachineTokenSource::new(url, "c".into(), &secret)?.with_clock(clock_fn);

    assert_eq!(src.token().await?, "t1");
    std::fs::write(&secret, "new\n")?;
    advance(&clock, Duration::from_secs(51));
    assert_eq!(src.token().await?, "t2");

    let reqs = captured.lock().await;
    let old = base64::engine::general_purpose::STANDARD.encode("c:old");
    let new = base64::engine::general_purpose::STANDARD.encode("c:new");
    assert!(reqs[0].contains(&old), "first fetch uses the old secret");
    assert!(reqs[1].contains(&new), "refresh re-reads the rotated secret");
    Ok(())
}

// --- fail closed -------------------------------------------------------------

#[tokio::test]
async fn endpoint_down_fails_closed() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "s");
    let src = MachineTokenSource::new(closed_port_url().await?, "c".into(), &secret)?;
    let err = match src.token().await {
        Ok(_) => anyhow::bail!("must fail with nothing listening"),
        Err(e) => e.to_string(),
    };
    assert!(err.contains(AUTH_UNAVAILABLE), "got: {err}");
    assert!(err.contains("client c"), "names the client: {err}");
    Ok(())
}

#[tokio::test]
async fn idp_error_status_fails_closed_without_token() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "s");
    let (url, _) = token_server(vec![(
        401,
        r#"{"error":"unauthorized_client","error_description":"Invalid client secret"}"#.into(),
    )])
    .await?;
    let src = MachineTokenSource::new(url, "c".into(), &secret)?;
    let err = match src.token().await {
        Ok(_) => anyhow::bail!("401 must fail"),
        Err(e) => e.to_string(),
    };
    assert!(err.contains(AUTH_UNAVAILABLE) && err.contains("401"), "got: {err}");
    assert!(err.contains("unauthorized_client"), "carries the IdP's error code: {err}");
    Ok(())
}

#[tokio::test]
async fn empty_access_token_fails_closed() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "s");
    let (url, _) = token_server(vec![(200, token_body("", Some(100)))]).await?;
    let src = MachineTokenSource::new(url, "c".into(), &secret)?;
    let err = match src.token().await {
        Ok(_) => anyhow::bail!("empty access_token must fail"),
        Err(e) => e.to_string(),
    };
    assert!(err.contains(AUTH_UNAVAILABLE) && err.contains("empty access token"), "got: {err}");
    Ok(())
}

#[tokio::test]
async fn missing_or_empty_secret_file_is_auth_unavailable() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    // The token server must never be contacted: zero responses, so a fetch
    // attempt would hang on accept and time out rather than pass.
    let (url, captured) = token_server(vec![]).await?;

    let missing = dir.path().join("nope");
    let src = MachineTokenSource::new(url.clone(), "c".into(), &missing)?;
    let err = match src.token().await {
        Ok(_) => anyhow::bail!("missing secret must fail"),
        Err(e) => e.to_string(),
    };
    assert!(err.contains(AUTH_UNAVAILABLE) && err.contains("unreadable"), "got: {err}");

    let empty = dir.path().join("empty");
    std::fs::write(&empty, "  \n")?;
    let src = MachineTokenSource::new(url, "c".into(), &empty)?;
    let err = match src.token().await {
        Ok(_) => anyhow::bail!("empty secret must fail"),
        Err(e) => e.to_string(),
    };
    assert!(err.contains(AUTH_UNAVAILABLE) && err.contains("is empty"), "got: {err}");
    assert!(captured.lock().await.is_empty(), "no exchange without a secret");
    Ok(())
}

#[tokio::test]
async fn refresh_failure_serves_cached_until_expiry() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let secret = secret_file(&dir, "s");
    let (url, captured) = token_server(vec![
        (200, token_body("t1", Some(1800))),
        (503, r#"{"error":"idp down"}"#.into()),
        (503, r#"{"error":"idp down"}"#.into()),
    ])
    .await?;
    let (clock, clock_fn) = test_clock();
    let src = MachineTokenSource::new(url, "c".into(), &secret)?.with_clock(clock_fn);

    assert_eq!(src.token().await?, "t1");
    // Past half-life the refresh fails: the still-valid token is served.
    advance(&clock, Duration::from_secs(901));
    assert_eq!(src.token().await?, "t1", "stale-but-unexpired token is served");
    assert_eq!(captured.lock().await.len(), 2);
    // Past expiry it is not: fail closed.
    advance(&clock, Duration::from_secs(900));
    let err = match src.token().await {
        Ok(t) => anyhow::bail!("expired token {t} must not be served"),
        Err(e) => e.to_string(),
    };
    assert!(err.contains(AUTH_UNAVAILABLE), "got: {err}");
    assert_eq!(captured.lock().await.len(), 3);
    Ok(())
}

// --- claims decode -----------------------------------------------------------

#[test]
fn parse_jwt_claims_audience_string_or_array() -> anyhow::Result<()> {
    let one = parse_jwt_claims(&jwt(serde_json::json!({ "aud": "a" })));
    assert_eq!(one.audience(), vec!["a"]);
    let many = parse_jwt_claims(&jwt(serde_json::json!({ "aud": ["a", "b"] })));
    assert_eq!(many.audience(), vec!["a", "b"]);
    let none = parse_jwt_claims(&jwt(serde_json::json!({})));
    assert!(none.audience().is_empty());
    Ok(())
}

#[test]
fn parse_jwt_claims_merges_roles_and_lifetime() -> anyhow::Result<()> {
    let c = parse_jwt_claims(&jwt(serde_json::json!({
        "realm_roles": ["gardenio-machine-accounts", "gardenio-operator", ""],
        "realm_access": { "roles": ["gardenio-operator", "default-roles-gardenio"] },
        "exp": 1000, "iat": 400
    })));
    assert_eq!(
        c.roles(),
        vec!["gardenio-machine-accounts", "gardenio-operator", "default-roles-gardenio"]
    );
    assert_eq!(c.exp_minus_iat(), 600);
    assert_eq!(c.lifetime(), Some(Duration::from_secs(600)));
    Ok(())
}

#[test]
fn parse_jwt_claims_tolerates_garbage() -> anyhow::Result<()> {
    for raw in [
        "",
        "opaque",
        "a.b",
        "a.!!!.c",
        &format!("a.{}.c", base64::engine::general_purpose::URL_SAFE_NO_PAD.encode("not json")),
    ] {
        let c = parse_jwt_claims(raw);
        assert!(c.azp.is_empty() && c.lifetime().is_none(), "raw={raw:?}");
    }
    Ok(())
}

#[test]
fn token_response_expires_in_shapes() -> anyhow::Result<()> {
    let int: TokenResponse = serde_json::from_str(r#"{"access_token":"t","expires_in":1800}"#)?;
    assert_eq!(int.expires_in(), Some(Duration::from_secs(1800)));
    let s: TokenResponse = serde_json::from_str(r#"{"access_token":"t","expires_in":"60"}"#)?;
    assert_eq!(s.expires_in(), Some(Duration::from_secs(60)));
    let zero: TokenResponse = serde_json::from_str(r#"{"access_token":"t","expires_in":0}"#)?;
    assert_eq!(zero.expires_in(), None);
    let absent: TokenResponse = serde_json::from_str(r#"{"access_token":"t"}"#)?;
    assert_eq!(absent.expires_in(), None);
    Ok(())
}
