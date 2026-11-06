// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Provider gating: a Claude session never receives, is rebalanced onto, or is
//! reassigned to a Codex account, and a Codex session never to a Claude one.

use std::collections::HashMap;
use std::sync::atomic::Ordering;
use std::sync::Arc;

use tokio::sync::broadcast;
use tokio_util::sync::CancellationToken;

use super::e2e_tests::{entry, mock_upstream, test_config};
use super::*;
use crate::credential::broker::CredentialBroker;
use crate::credential::{AccountConfig, CredentialConfig, CredentialEvent};

fn account(name: &str, provider: &str) -> AccountConfig {
    AccountConfig {
        name: name.to_owned(),
        provider: provider.to_owned(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }
}

fn codex_doc(access: &str) -> String {
    serde_json::json!({
        "auth_mode": "chatgpt",
        "tokens": { "access_token": access, "refresh_token": "rt", "id_token": "id" },
    })
    .to_string()
}

/// A mixed pool: `(name, provider)` accounts, each seeded with a token.
async fn mixed_state(accounts: &[(&str, &str)]) -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let configs = accounts.iter().map(|(n, p)| account(n, p)).collect();
    let broker = CredentialBroker::new(CredentialConfig { accounts: configs }, event_tx, None);
    for (name, provider) in accounts {
        let token = if *provider == "openai" {
            codex_doc(&format!("at-{name}"))
        } else {
            format!("tok-{name}")
        };
        broker.set_token(name, token, None, None).await.expect("seed token");
    }
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), broker))
}

fn agent(kind: &str) -> serde_json::Value {
    serde_json::json!({ "agent": kind })
}

fn codex_creds() -> HashMap<String, String> {
    HashMap::from([("CODEX_AUTH_JSON".to_owned(), codex_doc("at-x"))])
}

fn claude_creds() -> HashMap<String, String> {
    HashMap::from([("CLAUDE_CODE_OAUTH_TOKEN".to_owned(), "tok".to_owned())])
}

#[tokio::test]
async fn codex_profile_reaches_only_codex_sessions() -> anyhow::Result<()> {
    let state = mixed_state(&[("codex-a", "openai"), ("claude-a", "claude")]).await;
    let (claude_url, claude_hits) = mock_upstream("idle").await?;
    let (codex_url, codex_hits) = mock_upstream("idle").await?;
    let (bare_url, bare_hits) = mock_upstream("idle").await?;
    {
        let mut sessions = state.sessions.write().await;
        // `profiles_needed: ["*"]` must not override the provider gate.
        let mut claude_meta = agent("claude");
        claude_meta["profiles_needed"] = serde_json::json!(["*"]);
        sessions.insert("c".into(), entry("c", claude_url, Some("claude-a"), claude_meta));
        sessions.insert("x".into(), entry("x", codex_url, Some("codex-a"), agent("codex")));
        // No agent in metadata: treated as non-Codex.
        sessions
            .insert("b".into(), entry("b", bare_url, Some("claude-a"), serde_json::Value::Null));
    }

    distribute_to_sessions(&state, "codex-a", &codex_creds(), true).await;

    assert_eq!(claude_hits.profiles.load(Ordering::SeqCst), 0, "claude session got codex profile");
    assert_eq!(claude_hits.switch.load(Ordering::SeqCst), 0);
    assert_eq!(bare_hits.profiles.load(Ordering::SeqCst), 0, "agent-less session got codex");
    assert_eq!(codex_hits.profiles.load(Ordering::SeqCst), 1, "codex session missed its profile");
    Ok(())
}

#[tokio::test]
async fn claude_profile_never_reaches_codex_sessions() -> anyhow::Result<()> {
    let state = mixed_state(&[("codex-a", "openai"), ("claude-a", "claude")]).await;
    let (claude_url, claude_hits) = mock_upstream("idle").await?;
    let (codex_url, codex_hits) = mock_upstream("idle").await?;
    {
        let mut sessions = state.sessions.write().await;
        sessions.insert("c".into(), entry("c", claude_url, Some("claude-a"), agent("claude")));
        sessions.insert("x".into(), entry("x", codex_url, Some("codex-a"), agent("codex")));
    }

    distribute_to_sessions(&state, "claude-a", &claude_creds(), true).await;

    assert_eq!(codex_hits.profiles.load(Ordering::SeqCst), 0, "codex session got claude profile");
    assert_eq!(claude_hits.profiles.load(Ordering::SeqCst), 1);
    Ok(())
}

#[tokio::test]
async fn distribute_infers_codex_from_credentials_for_unknown_account() -> anyhow::Result<()> {
    // The broker no longer knows the account (removed mid-flight): the
    // CODEX_AUTH_JSON key still marks it as a Codex credential.
    let state = mixed_state(&[("claude-a", "claude")]).await;
    let (claude_url, claude_hits) = mock_upstream("idle").await?;
    state
        .sessions
        .write()
        .await
        .insert("c".into(), entry("c", claude_url, Some("claude-a"), agent("claude")));

    distribute_to_sessions(&state, "gone", &codex_creds(), false).await;

    assert_eq!(claude_hits.profiles.load(Ordering::SeqCst), 0);
    Ok(())
}

#[tokio::test]
async fn rebalance_never_moves_claude_session_onto_codex_account() -> anyhow::Result<()> {
    // The only other healthy account is a Codex one.
    let state = mixed_state(&[("claude-a", "claude"), ("codex-a", "openai")]).await;
    let (url, hits) = mock_upstream("idle").await?;
    let e = entry("c", url, Some("claude-a"), agent("claude"));
    state.sessions.write().await.insert("c".into(), Arc::clone(&e));

    rebalance_from_account(&state, "claude-a").await;

    assert_eq!(hits.switch.load(Ordering::SeqCst), 0, "claude session switched to codex");
    assert_eq!(e.assigned_account.read().await.as_deref(), Some("claude-a"));
    Ok(())
}

#[tokio::test]
async fn rebalance_picks_same_provider_target_in_mixed_pool() -> anyhow::Result<()> {
    let state =
        mixed_state(&[("claude-a", "claude"), ("claude-b", "claude"), ("codex-a", "openai")]).await;
    state.credential_broker.mark_throttled("claude-a").await;
    let (url, hits) = mock_upstream("idle").await?;
    let e = entry("c", url, Some("claude-a"), agent("claude"));
    state.sessions.write().await.insert("c".into(), Arc::clone(&e));

    rebalance_from_account(&state, "claude-a").await;

    assert_eq!(e.assigned_account.read().await.as_deref(), Some("claude-b"));
    assert_eq!(hits.switch.load(Ordering::SeqCst), 1);
    Ok(())
}

#[tokio::test]
async fn rebalance_never_moves_codex_session_onto_claude_account() -> anyhow::Result<()> {
    let state = mixed_state(&[("codex-a", "openai"), ("claude-a", "claude")]).await;
    let (url, hits) = mock_upstream("idle").await?;
    let e = entry("x", url, Some("codex-a"), agent("codex"));
    state.sessions.write().await.insert("x".into(), Arc::clone(&e));

    rebalance_from_account(&state, "codex-a").await;

    assert_eq!(hits.switch.load(Ordering::SeqCst), 0, "codex session switched to claude");
    assert_eq!(e.assigned_account.read().await.as_deref(), Some("codex-a"));
    Ok(())
}

#[tokio::test]
async fn rebalance_moves_codex_session_to_other_codex_account() -> anyhow::Result<()> {
    let state =
        mixed_state(&[("codex-a", "openai"), ("codex-b", "openai"), ("claude-a", "claude")]).await;
    state.credential_broker.mark_throttled("codex-a").await;
    let (url, _hits) = mock_upstream("idle").await?;
    let e = entry("x", url, Some("codex-a"), agent("codex"));
    state.sessions.write().await.insert("x".into(), Arc::clone(&e));

    rebalance_from_account(&state, "codex-a").await;

    assert_eq!(e.assigned_account.read().await.as_deref(), Some("codex-b"));
    Ok(())
}

#[tokio::test]
async fn reassign_session_refuses_cross_provider_move() -> anyhow::Result<()> {
    let state = mixed_state(&[("claude-a", "claude"), ("codex-a", "openai")]).await;
    let (url, hits) = mock_upstream("idle").await?;
    let e = entry("c", url, Some("claude-a"), agent("claude"));

    let moved = super::super::reassign::reassign_session(
        state.http_client.clone(),
        &state.credential_broker,
        &e,
        "claude-a",
        "codex-a",
        &codex_creds(),
    )
    .await;

    assert!(!moved);
    assert_eq!(hits.profiles.load(Ordering::SeqCst), 0);
    assert_eq!(hits.switch.load(Ordering::SeqCst), 0);
    assert_eq!(e.assigned_account.read().await.as_deref(), Some("claude-a"));
    Ok(())
}

#[test]
fn session_accepts_account_gates_provider_before_profiles_needed() {
    let mut claude = agent("claude");
    claude["profiles_needed"] = serde_json::json!(["codex-a"]);
    assert!(!session_accepts_account(&claude, "codex-a", "openai", None));
    assert!(session_accepts_account(&agent("codex"), "codex-a", "openai", None));
    assert!(!session_accepts_account(&agent("codex"), "claude-a", "claude", None));
    assert!(session_accepts_account(&agent("claude"), "claude-a", "anthropic", None));
}
