// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::tests::{make_broker, make_persisting_broker};
use super::*;
use crate::credential::AccountConfig;

// ── Codex (provider=openai, ChatGPT subscription) ──────────────────────

fn codex_doc(access: &str, refresh: &str) -> String {
    serde_json::json!({
        "auth_mode": "chatgpt",
        "tokens": {
            "id_token": "id",
            "access_token": access,
            "refresh_token": refresh,
            "account_id": "acct-1",
        },
    })
    .to_string()
}

fn make_codex_broker(
    model: Option<&str>,
) -> (Arc<CredentialBroker>, broadcast::Receiver<CredentialEvent>) {
    let config = AccountConfig {
        name: "codex-a".into(),
        provider: "openai".into(),
        env_key: None,
        model: model.map(str::to_owned),
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    };
    let (tx, rx) = broadcast::channel(16);
    (CredentialBroker::new(CredentialConfig { accounts: vec![config] }, tx, None), rx)
}

#[tokio::test]
async fn openai_account_distributes_auth_document_not_api_key_env() -> anyhow::Result<()> {
    let (broker, mut rx) = make_codex_broker(Some("gpt-5-codex"));
    broker.set_token("codex-a", codex_doc("at-1", "rt-real"), None, None).await?;

    let CredentialEvent::Refreshed { credentials, .. } = rx.recv().await? else {
        anyhow::bail!("expected Refreshed");
    };
    let mut keys: Vec<_> = credentials.keys().map(String::as_str).collect();
    keys.sort_unstable();
    assert_eq!(keys, ["CODEX_AUTH_JSON", "CODEX_MODEL"]);
    assert_eq!(credentials["CODEX_MODEL"], "gpt-5-codex");
    for forbidden in
        ["OPENAI_API_KEY", "ANTHROPIC_MODEL", "CLAUDE_MODEL", "ANTHROPIC_CUSTOM_HEADERS"]
    {
        assert!(!credentials.contains_key(forbidden), "{forbidden} leaked into codex creds");
    }
    // get_credentials agrees with what was distributed.
    assert_eq!(broker.get_credentials("codex-a").await.unwrap(), credentials);
    Ok(())
}

#[tokio::test]
async fn openai_account_never_distributes_real_refresh_token() -> anyhow::Result<()> {
    let (broker, mut rx) = make_codex_broker(None);
    broker
        .set_token(
            "codex-a",
            codex_doc("at-1", "rt-real-secret"),
            Some("rt-real-secret".into()),
            None,
        )
        .await?;
    let CredentialEvent::Refreshed { credentials, .. } = rx.recv().await? else {
        anyhow::bail!("expected Refreshed");
    };
    let doc = &credentials["CODEX_AUTH_JSON"];
    assert!(!doc.contains("rt-real-secret"));
    assert!(doc.contains(codex_auth::PLACEHOLDER_REFRESH_TOKEN));
    Ok(())
}

#[tokio::test]
async fn openai_rotation_redistributes_and_dedups() -> anyhow::Result<()> {
    let (broker, mut rx) = make_codex_broker(None);
    broker.set_token("codex-a", codex_doc("at-1", "rt-1"), None, None).await?;
    assert!(matches!(rx.recv().await?, CredentialEvent::Refreshed { .. }));

    // The keeper pushing the session form of the same login is not a change.
    let session = codex_doc("at-1", codex_auth::PLACEHOLDER_REFRESH_TOKEN);
    broker.set_token("codex-a", session, None, None).await?;
    assert!(rx.try_recv().is_err(), "identical document must not redistribute");

    // A rotated access token is a new credential and fans out.
    broker.set_token("codex-a", codex_doc("at-2", "rt-2"), None, None).await?;
    let CredentialEvent::Refreshed { credentials, .. } = rx.recv().await? else {
        anyhow::bail!("expected Refreshed");
    };
    assert!(credentials["CODEX_AUTH_JSON"].contains("at-2"));
    Ok(())
}

#[tokio::test]
async fn openai_expires_in_recorded_as_expires_at() -> anyhow::Result<()> {
    let (broker, _rx) = make_codex_broker(None);
    broker.set_token("codex-a", codex_doc("at-1", "rt"), None, Some(3600)).await?;
    let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)?.as_secs();
    let expires_at = broker.status_list().await[0].expires_at.expect("expires_at set");
    assert!((now + 3590..=now + 3610).contains(&expires_at));
    Ok(())
}

#[tokio::test]
async fn openai_rejects_plain_api_key_and_keeps_old_credential() -> anyhow::Result<()> {
    let (broker, _rx) = make_codex_broker(None);
    broker.set_token("codex-a", codex_doc("at-1", "rt"), None, None).await?;
    let err = broker.set_token("codex-a", "sk-proj-abc".into(), None, None).await.unwrap_err();
    assert!(err.to_string().contains("not JSON"), "{err}");
    assert!(broker.get_credentials("codex-a").await.unwrap()["CODEX_AUTH_JSON"].contains("at-1"));
    Ok(())
}

#[tokio::test]
async fn claude_accounts_unchanged_by_codex_path() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "sk-ant-oat-x".into(), Some("ignored".into()), Some(60)).await?;
    let creds = broker.get_credentials("acct-a").await.unwrap();
    assert_eq!(creds["CLAUDE_CODE_OAUTH_TOKEN"], "sk-ant-oat-x");
    assert_eq!(broker.status_list().await[0].expires_at, None);
    Ok(())
}

#[serial_test::serial]
#[tokio::test]
async fn openai_add_account_persists_expiry_and_restores() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    std::env::set_var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP", "1");
    let broker = make_persisting_broker(dir.path().to_path_buf(), vec![("codex-a", "openai")]);
    broker.set_token("codex-a", codex_doc("at-1", "rt"), None, Some(1800)).await?;
    broker.wait_for_persist_idle().await;

    let loaded = crate::credential::persist::load(&dir.path().join("credentials.json"))?;
    assert!(loaded.accounts["codex-a"].expires_at > 0);
    assert!(!loaded.accounts["codex-a"].access_token.contains("\"rt\""));

    let fresh = make_persisting_broker(dir.path().to_path_buf(), vec![("codex-a", "openai")]);
    fresh.load_persisted(&loaded).await;
    assert!(fresh.status_list().await[0].expires_at.is_some());
    assert!(fresh.get_credentials("codex-a").await.unwrap().contains_key("CODEX_AUTH_JSON"));
    std::env::remove_var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP");
    Ok(())
}

// ── Provider-aware account selection ───────────────────────────────────

async fn mixed_pool() -> anyhow::Result<Arc<CredentialBroker>> {
    let broker =
        make_broker(vec![("claude-a", "claude"), ("claude-b", "claude"), ("codex-a", "openai")]);
    broker.set_token("claude-a", "ka".into(), None, None).await?;
    broker.set_token("claude-b", "kb".into(), None, None).await?;
    broker.set_token("codex-a", codex_doc("at", "rt"), None, None).await?;
    Ok(broker)
}

#[tokio::test]
async fn default_assignment_never_hands_out_codex_account() -> anyhow::Result<()> {
    let broker = mixed_pool().await?;
    // Load the Claude accounts so least-loaded would otherwise prefer codex-a.
    for _ in 0..3 {
        broker.session_assigned("claude-a").await;
        broker.session_assigned("claude-b").await;
    }
    for _ in 0..4 {
        let picked = broker.assign_account(None).await;
        assert_ne!(picked.as_deref(), Some("codex-a"));
        assert!(picked.is_some());
    }
    let authorized: std::collections::HashSet<String> =
        ["codex-a", "claude-a"].map(str::to_owned).into();
    assert_eq!(
        broker.assign_account_for_project(None, &authorized).await.as_deref(),
        Some("claude-a")
    );
    // A preferred Codex account is ignored for a session with no Codex family.
    assert_ne!(broker.assign_account(Some("codex-a")).await.as_deref(), Some("codex-a"));
    Ok(())
}

#[tokio::test]
async fn claude_family_never_gets_codex_and_codex_family_only_codex() -> anyhow::Result<()> {
    let broker = mixed_pool().await?;
    for _ in 0..3 {
        broker.session_assigned("claude-a").await;
        broker.session_assigned("claude-b").await;
    }
    let claude = Some(ProviderFamily::Claude);
    let codex = Some(ProviderFamily::Codex);

    for _ in 0..4 {
        assert_ne!(
            broker.assign_account_for_family(None, claude).await.as_deref(),
            Some("codex-a")
        );
    }
    assert_eq!(
        broker.assign_account_for_family(Some("claude-a"), codex).await.as_deref(),
        Some("codex-a"),
        "preferred claude account must be ignored for a codex session"
    );
    assert_eq!(broker.assign_account_for_family(None, codex).await.as_deref(), Some("codex-a"));

    let all: std::collections::HashSet<String> =
        ["claude-a", "claude-b", "codex-a"].map(str::to_owned).into();
    assert_eq!(
        broker.assign_account_for_project_family(Some("claude-a"), &all, codex).await.as_deref(),
        Some("codex-a")
    );
    assert_ne!(
        broker.assign_account_for_project_family(Some("codex-a"), &all, claude).await.as_deref(),
        Some("codex-a")
    );
    // No Codex account authorized -> a codex session gets nothing.
    let only_claude: std::collections::HashSet<String> =
        ["claude-a", "claude-b"].map(str::to_owned).into();
    assert_eq!(broker.assign_account_for_project_family(None, &only_claude, codex).await, None);
    Ok(())
}

#[tokio::test]
async fn model_env_is_only_pushed_for_claude_accounts() -> anyhow::Result<()> {
    let mut configs = Vec::new();
    for (name, provider) in [("c", "claude"), ("g", "gemini")] {
        configs.push(AccountConfig {
            name: name.into(),
            provider: provider.into(),
            env_key: None,
            model: Some("some-model".into()),
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        });
    }
    let (tx, mut rx) = broadcast::channel(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: Vec::new() }, tx, None);
    // add_account (dynamic) is the path that previously sent only the token.
    broker.add_account(configs[0].clone(), Some("tok".into()), None, None).await?;
    broker.add_account(configs[1].clone(), Some("tok".into()), None, None).await?;

    let CredentialEvent::Refreshed { credentials: claude, .. } = rx.recv().await? else {
        anyhow::bail!("expected Refreshed");
    };
    let CredentialEvent::Refreshed { credentials: gemini, .. } = rx.recv().await? else {
        anyhow::bail!("expected Refreshed");
    };
    assert_eq!(claude["ANTHROPIC_MODEL"], "some-model");
    assert_eq!(claude["CLAUDE_MODEL"], "some-model");
    assert!(!gemini.contains_key("ANTHROPIC_MODEL") && !gemini.contains_key("CLAUDE_MODEL"));
    Ok(())
}
