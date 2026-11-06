// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
//
// Tests for the claimed-bead title enricher in lib.rs. `enrich_claimed_titles`
// is driven against a mock beads daemon so the two-hop lookup
// (session → agent bead → task_id → task bead → title) is exercised end-to-end.
// `spawn_account_cache_refresh` is also driven end-to-end against a mock beads
// daemon serving the account/config/role list endpoints.

use std::collections::HashMap;
use std::sync::atomic::AtomicU32;
use std::sync::Arc;
use std::time::Duration;

use axum::extract::Query;
use axum::routing::get;
use axum::{Json, Router};
use serial_test::serial;
use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;

use super::*;
use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::{CredentialConfig, CredentialEvent};
use crate::state::{MuxEvent, SessionEntry, SessionTransport};

fn test_config() -> MuxConfig {
    MuxConfig {
        host: "127.0.0.1".into(),
        port: 0,
        auth_token: None,
        oidc_issuer_url: None,
        oidc_client_id: None,
        screen_poll_ms: 500,
        status_poll_ms: 2000,
        health_check_ms: 10000,
        max_health_failures: 3,
        launch: None,
        credential_config: None,
        api_key_file: None,
        prewarm_capacity: 64,
        prewarm_poll_ms: 15000,
        state_dir: None,
        dev_mode: false,
        base_path: None,
        #[cfg(debug_assertions)]
        hot: false,
    }
}

fn test_state() -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, event_tx, None);
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), broker))
}

fn entry_with_metadata(id: &str, metadata: serde_json::Value) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url: "http://127.0.0.1:9".to_owned(),
        auth_token: None,
        metadata,
        registered_at: std::time::Instant::now(),
        cached_screen: RwLock::new(None),
        cached_status: RwLock::new(None),
        cached_usage: RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: RwLock::new(None),
        assigned_account: RwLock::new(None),
        claimed_bead_title: RwLock::new(None),
        transport: SessionTransport::default(),
    })
}

#[derive(serde::Deserialize)]
struct IdsQuery {
    ids: String,
}

/// Build a batch-beads response body for the requested ids, drawing bead
/// title + fields from the supplied fixtures.
fn batch_response(ids: &str, fixtures: &[(&str, &str, serde_json::Value)]) -> serde_json::Value {
    let mut results = Vec::new();
    for id in ids.split(',') {
        if let Some((_, title, fields)) = fixtures.iter().find(|(fid, _, _)| *fid == id) {
            results.push(serde_json::json!({
                "id": id,
                "bead": { "id": id, "title": title, "fields": fields }
            }));
        } else {
            results.push(serde_json::json!({ "id": id, "error": "not found" }));
        }
    }
    serde_json::json!({ "results": results })
}

/// Spawn a mock beads daemon. It serves `/v1/batch/beads?ids=` for both the
/// agent-bead hop (returns a `task_id` field) and the task-bead hop (returns a
/// title), keyed by the fixture tables baked into the closure.
async fn mock_beads() -> anyhow::Result<String> {
    // Agent beads: agent_bead_id → task_id field.
    // Task beads:  task_id → human title.
    let app = Router::new().route(
        "/v1/batch/beads",
        get(|Query(q): Query<IdsQuery>| async move {
            let fixtures: Vec<(&str, &str, serde_json::Value)> = vec![
                // Agent beads carry a task_id field (title unused for these).
                ("agent-1", "", serde_json::json!({ "task_id": "task-1" })),
                ("agent-2", "", serde_json::json!({ "task_id": "task-2" })),
                // Task beads carry the human-facing title.
                ("task-1", "Fix the flux capacitor", serde_json::json!({})),
                ("task-2", "Refactor the warp core", serde_json::json!({})),
            ];
            Json(batch_response(&q.ids, &fixtures))
        }),
    );
    // BeadsClient::new builds a reqwest client requiring the rustls crypto
    // provider; install it here so tests pass when run in isolation.
    let _ = rustls::crypto::ring::default_provider().install_default();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(format!("http://{addr}"))
}

#[tokio::test]
async fn enrich_sets_claimed_bead_title_from_task_bead() -> anyhow::Result<()> {
    let url = mock_beads().await?;
    let client = crate::beads::BeadsClient::new(url, None)?;
    let state = test_state();

    // Session s1 carries agent_bead_id=agent-1 → task-1 → "Fix the flux capacitor".
    let e = entry_with_metadata("s1", serde_json::json!({ "agent_bead_id": "agent-1" }));
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    enrich_claimed_titles(&state, &client).await;

    let title = e.claimed_bead_title.read().await.clone();
    assert_eq!(title.as_deref(), Some("Fix the flux capacitor"));
    Ok(())
}

#[tokio::test]
async fn enrich_handles_multiple_sessions() -> anyhow::Result<()> {
    let url = mock_beads().await?;
    let client = crate::beads::BeadsClient::new(url, None)?;
    let state = test_state();

    let e1 = entry_with_metadata("s1", serde_json::json!({ "agent_bead_id": "agent-1" }));
    let e2 = entry_with_metadata("s2", serde_json::json!({ "agent_bead_id": "agent-2" }));
    {
        let mut sessions = state.sessions.write().await;
        sessions.insert("s1".to_owned(), Arc::clone(&e1));
        sessions.insert("s2".to_owned(), Arc::clone(&e2));
    }

    enrich_claimed_titles(&state, &client).await;

    assert_eq!(e1.claimed_bead_title.read().await.as_deref(), Some("Fix the flux capacitor"));
    assert_eq!(e2.claimed_bead_title.read().await.as_deref(), Some("Refactor the warp core"));
    Ok(())
}

#[tokio::test]
async fn enrich_noop_without_agent_bead_id() -> anyhow::Result<()> {
    let url = mock_beads().await?;
    let client = crate::beads::BeadsClient::new(url, None)?;
    let state = test_state();

    // No agent_bead_id in metadata → nothing to enrich, title stays None.
    let e = entry_with_metadata("s1", serde_json::json!({ "other": "x" }));
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    enrich_claimed_titles(&state, &client).await;
    assert!(e.claimed_bead_title.read().await.is_none());
    Ok(())
}

#[tokio::test]
async fn enrich_noop_when_agent_bead_missing_task_id() -> anyhow::Result<()> {
    let url = mock_beads().await?;
    let client = crate::beads::BeadsClient::new(url, None)?;
    let state = test_state();

    // agent_bead_id points at an unknown agent bead (no task_id resolvable).
    let e = entry_with_metadata("s1", serde_json::json!({ "agent_bead_id": "agent-unknown" }));
    state.sessions.write().await.insert("s1".to_owned(), Arc::clone(&e));

    enrich_claimed_titles(&state, &client).await;
    assert!(e.claimed_bead_title.read().await.is_none());
    Ok(())
}

#[tokio::test]
async fn enrich_empty_sessions_is_noop() -> anyhow::Result<()> {
    let url = mock_beads().await?;
    let client = crate::beads::BeadsClient::new(url, None)?;
    let state = test_state();
    // No sessions at all — returns early cleanly.
    enrich_claimed_titles(&state, &client).await;
    Ok(())
}

// ── spawn_account_cache_refresh (end-to-end over the beads list endpoints) ──

/// Mock beads daemon serving the three list endpoints `AccountCache::refresh`
/// hits: `/v1/beads?type=account`, `?type=config`, and `?type=role`. Returns
/// one account bead (with probe fields, so a ProbeUpdated event fires) and one
/// config:auth bead mapping a project to that account.
async fn mock_beads_lists() -> anyhow::Result<String> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let app = Router::new().route(
        "/v1/beads",
        get(|Query(q): Query<HashMap<String, String>>| async move {
            let kind = q.get("type").map(String::as_str).unwrap_or_default();
            let beads = match kind {
                "account" => serde_json::json!([{
                    "id": "bead-acct-a",
                    "title": "acct-a",
                    "fields": {
                        "provider": "claude",
                        "status": "active",
                        "throttle_status": "ok",
                        "utilization_5h": "12",
                        "utilization_7d": "34"
                    },
                    "labels": ["project:proj"]
                }]),
                "config" => serde_json::json!([{
                    "id": "bead-cfg",
                    "title": "auth",
                    "description": "{\"account\":\"acct-a\"}",
                    "fields": {},
                    "labels": ["project:proj"]
                }]),
                // role beads: none configured.
                _ => serde_json::json!([]),
            };
            Json(serde_json::json!({ "beads": beads }))
        }),
    );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(format!("http://{addr}"))
}

#[tokio::test]
#[serial]
async fn account_cache_refresh_populates_cache_and_emits_probe_event() -> anyhow::Result<()> {
    // Drive spawn_account_cache_refresh end-to-end: it builds a BeadsClient
    // from BEADS_HTTP_ADDR, stores it on state, refreshes the account cache
    // from the mock daemon, and emits a ProbeUpdated event for the account
    // whose probe fields are non-empty. `#[serial]` guards the process-global
    // BEADS_HTTP_ADDR env var.
    let url = mock_beads_lists().await?;
    std::env::set_var("BEADS_HTTP_ADDR", &url);

    let state = test_state();
    let shutdown = CancellationToken::new();
    let mut events = state.feed.event_tx.subscribe();

    spawn_account_cache_refresh(Arc::clone(&state), shutdown.clone());

    // Wait for the cache to populate (first refresh runs immediately).
    let mut populated = false;
    for _ in 0..200 {
        if state.account_cache.read().await.all_accounts().contains_key("acct-a") {
            populated = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(populated, "account cache should populate from the mock beads daemon");

    // The account carries probe fields, so a ProbeUpdated event should fire.
    let mut saw_probe = false;
    for _ in 0..40 {
        match tokio::time::timeout(Duration::from_millis(50), events.recv()).await {
            Ok(Ok(MuxEvent::ProbeUpdated { account, utilization_5h, .. })) => {
                if account == "acct-a" && utilization_5h == "12" {
                    saw_probe = true;
                    break;
                }
            }
            Ok(Ok(_)) => continue,
            _ => break,
        }
    }
    assert!(saw_probe, "a ProbeUpdated event should fire for the account with probe data");

    // The beads client is stored on state for other components to reuse.
    assert!(state.beads_client.read().await.is_some(), "beads client stored on state");

    // The project→account mapping is cached from the config:auth bead.
    assert_eq!(
        state.account_cache.read().await.account_for_project("proj").as_deref(),
        Some("acct-a")
    );

    shutdown.cancel();
    std::env::remove_var("BEADS_HTTP_ADDR");
    Ok(())
}

/// Reads use the machine principal when one is configured; otherwise they fall
/// back to the static-token client (gd-in7EZvvd3T).
#[tokio::test]
async fn read_client_prefers_machine_principal() -> anyhow::Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let dir = tempfile::tempdir()?;
    let source = crate::machine_token::MachineTokenSource::new(
        "http://127.0.0.1:9/token".into(),
        "gardenio-coopmux".into(),
        dir.path().join("clientSecret"),
    )?;
    let machine = Arc::new(crate::beads::BeadsClient::new_machine(
        "http://127.0.0.1:9".into(),
        Arc::new(source),
    )?);
    let fallback =
        Arc::new(crate::beads::BeadsClient::new("http://127.0.0.1:9".into(), Some("tok".into()))?);

    let mut state = MuxState::new(
        test_config(),
        CancellationToken::new(),
        CredentialBroker::new(
            CredentialConfig { accounts: vec![] },
            broadcast::channel::<CredentialEvent>(16).0,
            None,
        ),
    );
    assert!(!read_client(&state, Arc::clone(&fallback)).is_machine(), "no principal: static");

    state.machine_beads_client = Some(machine);
    assert!(read_client(&state, fallback).is_machine(), "principal configured: machine");
    Ok(())
}
