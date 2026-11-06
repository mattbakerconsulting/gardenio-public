// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

#[test]
fn user_agent_is_non_empty_and_well_formed() -> anyhow::Result<()> {
    assert!(!OIDC_USER_AGENT.is_empty(), "OIDC user agent must not be empty");
    assert!(
        OIDC_USER_AGENT.starts_with("coopmux/"),
        "expected coopmux/<version>, got {OIDC_USER_AGENT}"
    );
    // Must be > "coopmux/" prefix alone — i.e. carry a version suffix.
    assert!(
        OIDC_USER_AGENT.len() > "coopmux/".len(),
        "OIDC user agent missing version: {OIDC_USER_AGENT}"
    );
    Ok(())
}

#[test]
fn configured_client_sends_user_agent_header() -> anyhow::Result<()> {
    // reqwest::Client::builder() requires a rustls crypto provider; install
    // the workspace default if no other test already did.
    let _ = rustls::crypto::ring::default_provider().install_default();
    // Spin up a local TCP listener, accept one connection, capture bytes,
    // and assert the configured reqwest client included the OIDC UA on the
    // wire. This guards against future builder regressions that might drop
    // the default header.
    let rt = tokio::runtime::Builder::new_current_thread().enable_all().build()?;
    rt.block_on(async {
        let listener = TcpListener::bind("127.0.0.1:0").await?;
        let addr = listener.local_addr()?;

        let server = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await?;
            let mut buf = [0u8; 4096];
            let mut accumulated = Vec::new();
            loop {
                let n = socket.read(&mut buf).await?;
                if n == 0 {
                    break;
                }
                accumulated.extend_from_slice(&buf[..n]);
                if accumulated.windows(4).any(|w| w == b"\r\n\r\n") {
                    break;
                }
            }
            socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n").await?;
            Ok::<_, anyhow::Error>(accumulated)
        });

        let client = reqwest::Client::builder().user_agent(OIDC_USER_AGENT).build()?;
        let _ = client.get(format!("http://{addr}/probe")).send().await?;

        let request_bytes = server.await??;
        let request_str = String::from_utf8_lossy(&request_bytes).to_lowercase();
        assert!(
            request_str.contains("user-agent: coopmux/"),
            "expected User-Agent header on the wire, got: {request_str}"
        );
        Ok::<_, anyhow::Error>(())
    })?;
    Ok(())
}

#[test]
fn test_bad_token_header() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    // A non-JWT string should fail early in decode_header.
    let rt = tokio::runtime::Builder::new_current_thread().build()?;
    let verifier = OidcVerifier {
        client: reqwest::Client::new(),
        jwks_uri: String::new(),
        audience: "test".into(),
        issuer: "https://example.com".into(),
        jwks: Arc::new(RwLock::new(jsonwebtoken::jwk::JwkSet { keys: vec![] })),
    };

    let result = rt.block_on(verifier.verify("not-a-jwt"));
    assert!(result.is_err());
    Ok(())
}
