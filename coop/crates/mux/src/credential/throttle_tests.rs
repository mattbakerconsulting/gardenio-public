// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use std::time::Duration;

use serial_test::serial;
use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;

use super::*;
use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::failover::{AccountInfo, AuthConfigInfo};
use crate::credential::{AccountConfig, CredentialConfig, CredentialEvent};
use crate::state::{MuxEvent, MuxState, SessionEntry, SessionTransport};

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

fn account(name: &str) -> AccountConfig {
    AccountConfig {
        name: name.to_owned(),
        provider: "claude".to_owned(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }
}

/// Build a state whose broker holds the given healthy accounts.
async fn state_with_accounts(names: &[&str]) -> Arc<MuxState> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let (event_tx, _) = broadcast::channel::<CredentialEvent>(16);
    let configs: Vec<AccountConfig> = names.iter().map(|n| account(n)).collect();
    let broker = CredentialBroker::new(CredentialConfig { accounts: configs }, event_tx, None);
    // Seed a token so each account is Healthy (Missing → Healthy on set_token).
    for n in names {
        broker.set_token(n, format!("tok-{n}"), None, None).await.expect("seed token");
    }
    Arc::new(MuxState::new(test_config(), CancellationToken::new(), broker))
}

fn session_entry(id: &str, assigned: Option<&str>) -> Arc<SessionEntry> {
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url: "http://127.0.0.1:9".to_owned(),
        auth_token: None,
        metadata: serde_json::Value::Null,
        registered_at: std::time::Instant::now(),
        cached_screen: RwLock::new(None),
        cached_status: RwLock::new(None),
        cached_usage: RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: RwLock::new(None),
        assigned_account: RwLock::new(assigned.map(str::to_owned)),
        claimed_bead_title: RwLock::new(None),
        transport: SessionTransport::default(),
    })
}

/// A session entry with a chosen upstream URL and an optional `project`
/// metadata tag (needed for project-scoped rebalance, which skips sessions
/// with no project field).
fn session_entry_full(
    id: &str,
    assigned: Option<&str>,
    url: String,
    project: Option<&str>,
) -> Arc<SessionEntry> {
    let metadata = match project {
        Some(p) => serde_json::json!({ "project": p }),
        None => serde_json::Value::Null,
    };
    Arc::new(SessionEntry {
        id: id.to_owned(),
        url,
        auth_token: None,
        metadata,
        registered_at: std::time::Instant::now(),
        cached_screen: RwLock::new(None),
        cached_status: RwLock::new(None),
        cached_usage: RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: RwLock::new(None),
        assigned_account: RwLock::new(assigned.map(str::to_owned)),
        claimed_bead_title: RwLock::new(None),
        transport: SessionTransport::Http,
    })
}

/// Seed `state.account_cache` so `project` currently uses `from` with a
/// fallback chain `from → to`, and `to` is a usable ("active") account.
/// This is the minimum shape `project_scoped_rebalance` needs to find a
/// healthy fallback for the project and reassign its sessions. Non-empty
/// bead_ids let the beads-update branch address a real PATCH endpoint.
async fn seed_rebalanceable_cache(state: &MuxState, from: &str, to: &str, project: &str) {
    let mut cache = state.account_cache.write().await;
    cache.accounts_mut().insert(
        from.to_owned(),
        AccountInfo {
            name: from.to_owned(),
            bead_id: format!("bead-{from}"),
            fallback: Some(to.to_owned()),
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            ..Default::default()
        },
    );
    cache.accounts_mut().insert(
        to.to_owned(),
        AccountInfo {
            name: to.to_owned(),
            bead_id: format!("bead-{to}"),
            fallback: None,
            provider: "claude".to_owned(),
            status: "active".to_owned(),
            ..Default::default()
        },
    );
    cache.auth_configs_mut().insert(
        project.to_owned(),
        AuthConfigInfo {
            bead_id: format!("bead-cfg-{project}"),
            account: from.to_owned(),
            primary_account: None,
            project: project.to_owned(),
        },
    );
}

/// Mock beads daemon that accepts the PATCH `/v1/beads/{id}` call the
/// beads-update tail of `project_scoped_rebalance` issues (exhausted-account
/// marking). Returns OK so `cache_updates` is populated and
/// `handle_throttle` reports a rebalance.
async fn mock_beads_patch() -> anyhow::Result<String> {
    use axum::routing::patch;
    use axum::{Json, Router};

    let _ = rustls::crypto::ring::default_provider().install_default();
    let app = Router::new()
        .route("/v1/beads/{id}", patch(|| async { Json(serde_json::json!({ "ok": true })) }));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok(format!("http://{addr}"))
}

/// Mock upstream coop accepting the profile-push + switch POSTs that
/// `reassign_session` issues. Returns the base URL and a counter of switch
/// calls so tests can assert the session was actually moved.
async fn mock_switch_upstream() -> anyhow::Result<(String, Arc<AtomicU32>)> {
    use axum::routing::post;
    use axum::{Json, Router};

    let switches = Arc::new(AtomicU32::new(0));
    let sw = Arc::clone(&switches);
    let app = Router::new()
        .route("/api/v1/session/profiles", post(|| async { Json(serde_json::json!({})) }))
        .route(
            "/api/v1/session/switch",
            post(move || {
                let sw = Arc::clone(&sw);
                async move {
                    sw.fetch_add(1, Ordering::SeqCst);
                    Json(serde_json::json!({ "ok": true }))
                }
            }),
        );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok((format!("http://{addr}"), switches))
}

/// Mock upstream coop that ACCEPTS the TCP connection but never sends an HTTP
/// response — modelling a parked/hung session pod (the pods being rebalanced in
/// a throttle storm are precisely the slow/unresponsive ones). A `reassign_session`
/// POST here blocks until its per-request timeout fires; without that timeout it
/// would hang forever. Returns the base URL; the spawned task holds each accepted
/// connection open until the test ends.
async fn mock_hung_upstream() -> anyhow::Result<String> {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    tokio::spawn(async move {
        let mut held = Vec::new();
        loop {
            match listener.accept().await {
                // Keep the accepted stream alive (never read/write) so the client
                // sits waiting for a response body it will never get.
                Ok((stream, _)) => held.push(stream),
                Err(_) => break,
            }
        }
    });
    Ok(format!("http://{addr}"))
}

async fn register(state: &MuxState, entry: Arc<SessionEntry>) {
    state.sessions.write().await.insert(entry.id.clone(), entry);
}

fn transition(session: &str, next: &str, error_category: Option<&str>) -> MuxEvent {
    MuxEvent::Transition {
        session: session.to_owned(),
        prev: "idle".to_owned(),
        next: next.to_owned(),
        seq: 1,
        cause: String::new(),
        last_message: None,
        prompt: None,
        error_detail: None,
        error_category: error_category.map(str::to_owned),
        parked_reason: None,
        resume_at_epoch_ms: None,
    }
}

// ── constant tables ────────────────────────────────────────────────────────

#[test]
fn throttle_categories_cover_known_signals() {
    assert!(THROTTLE_CATEGORIES.contains(&"rate_limited"));
    assert!(THROTTLE_CATEGORIES.contains(&"budget_exceeded"));
    assert!(!THROTTLE_CATEGORIES.contains(&"idle"));
}

#[test]
fn working_states_cover_active_states() {
    for s in ["working", "running", "streaming", "tool_use"] {
        assert!(WORKING_STATES.contains(&s), "{s} should count as working");
    }
    assert!(!WORKING_STATES.contains(&"idle"));
}

#[test]
#[serial]
fn failover_disabled_reads_env() {
    // Default: unset → not disabled. `#[serial]` because env mutation is
    // process-global and would otherwise race other tests reading env.
    std::env::remove_var("COOPMUX_DISABLE_FAILOVER");
    assert!(!failover_disabled());
    std::env::set_var("COOPMUX_DISABLE_FAILOVER", "1");
    assert!(failover_disabled());
    std::env::remove_var("COOPMUX_DISABLE_FAILOVER");
    assert!(!failover_disabled());
}

// ── lookup_session_account ─────────────────────────────────────────────────

#[tokio::test]
async fn lookup_returns_assigned_account() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", Some("acct-a"))).await;
    assert_eq!(lookup_session_account(&state, "s1").await, Some("acct-a".to_owned()));
    Ok(())
}

#[tokio::test]
async fn lookup_unknown_session_is_none() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    assert_eq!(lookup_session_account(&state, "nope").await, None);
    Ok(())
}

#[tokio::test]
async fn lookup_unassigned_session_is_none() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", None)).await;
    assert_eq!(lookup_session_account(&state, "s1").await, None);
    Ok(())
}

// ── handle_throttle ────────────────────────────────────────────────────────

#[tokio::test]
async fn handle_throttle_marks_account_throttled() -> anyhow::Result<()> {
    // With an empty account_cache, project_scoped_rebalance finds no affected
    // projects and returns an empty map (no rebalance), so we can assert the
    // broker bookkeeping side — which is what observability/failover reads —
    // without any env manipulation.
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", Some("acct-a"))).await;
    assert_eq!(state.credential_broker.account_status("acct-a").await, "healthy");

    let rebalanced = handle_throttle(&state, "s1", "rate_limited", Some("429")).await;
    assert!(!rebalanced, "empty cache → no project rebalance reported");
    assert_eq!(state.credential_broker.account_status("acct-a").await, "throttled");
    Ok(())
}

#[tokio::test]
async fn handle_throttle_unknown_session_is_noop() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    // No session registered → lookup returns None → early return, account stays healthy.
    let rebalanced = handle_throttle(&state, "ghost", "budget_exceeded", None).await;
    assert!(!rebalanced);
    assert_eq!(state.credential_broker.account_status("acct-a").await, "healthy");
    Ok(())
}

// ── handle_recovery ────────────────────────────────────────────────────────

#[tokio::test]
async fn handle_recovery_clears_throttle() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", Some("acct-a"))).await;

    // Put the account into throttled state first.
    state.credential_broker.mark_throttled("acct-a").await;
    assert_eq!(state.credential_broker.account_status("acct-a").await, "throttled");

    handle_recovery(&state, "s1").await;
    assert_eq!(
        state.credential_broker.account_status("acct-a").await,
        "healthy",
        "recovery should clear the throttle"
    );
    Ok(())
}

#[tokio::test]
async fn handle_recovery_noop_when_not_throttled() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", Some("acct-a"))).await;
    // Account is healthy; recovery clears nothing and must not error.
    handle_recovery(&state, "s1").await;
    assert_eq!(state.credential_broker.account_status("acct-a").await, "healthy");
    Ok(())
}

#[tokio::test]
async fn handle_recovery_unknown_session_is_noop() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    state.credential_broker.mark_throttled("acct-a").await;
    handle_recovery(&state, "ghost").await;
    // No session → no account resolved → still throttled.
    assert_eq!(state.credential_broker.account_status("acct-a").await, "throttled");
    Ok(())
}

#[tokio::test]
async fn handle_recovery_preserves_controller_warming() -> anyhow::Result<()> {
    // A working-state transition on a surviving session of a WARMING account
    // must NOT clear the controller-owned soft-steer state. `Warming` is set by
    // the ratelimit probe's `account_warming` NATS event and may only be cleared
    // by its `account_cooled` counterpart (`clear_warming`). If the local
    // recovery path clobbered it, a routine idle→working transition would
    // re-admit new sessions onto a near-capacity account, defeating soft
    // failover (ka-0000003184).
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", Some("acct-a"))).await;

    // Controller marks the account warming (approaching its rate cap). Existing
    // sessions keep running; only new-session assignment is steered away.
    let marked = state.credential_broker.mark_warming("acct-a").await;
    assert!(marked, "account should transition Healthy → Warming");
    assert_eq!(state.credential_broker.account_status("acct-a").await, "warming");

    // A surviving session transitions to a working state — the local recovery
    // path fires. It must leave the warming state intact.
    handle_recovery(&state, "s1").await;
    assert_eq!(
        state.credential_broker.account_status("acct-a").await,
        "warming",
        "local recovery must not clear controller-owned warming state"
    );

    // Only the controller's cooled event clears it.
    assert!(state.credential_broker.clear_warming("acct-a").await);
    assert_eq!(state.credential_broker.account_status("acct-a").await, "healthy");
    Ok(())
}

// ── spawn_throttle_watcher (end-to-end over the event channel) ─────────────

#[tokio::test]
async fn watcher_marks_throttled_on_rate_limit_transition() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", Some("acct-a"))).await;

    spawn_throttle_watcher(Arc::clone(&state));

    // Emit a throttle transition on the feed.
    state.feed.event_tx.send(transition("s1", "idle", Some("rate_limited")))?;

    let mut throttled = false;
    for _ in 0..200 {
        if state.credential_broker.account_status("acct-a").await == "throttled" {
            throttled = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(throttled, "watcher should mark the account throttled on a rate_limited transition");
    Ok(())
}

#[tokio::test]
async fn watcher_recovers_account_on_working_transition() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", Some("acct-a"))).await;
    state.credential_broker.mark_throttled("acct-a").await;

    spawn_throttle_watcher(Arc::clone(&state));

    // A working transition (no error_category) should trigger recovery.
    state.feed.event_tx.send(transition("s1", "working", None))?;

    let mut recovered = false;
    for _ in 0..200 {
        if state.credential_broker.account_status("acct-a").await == "healthy" {
            recovered = true;
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert!(recovered, "watcher should clear throttle when a session starts working again");
    Ok(())
}

#[tokio::test]
async fn watcher_ignores_non_throttle_transitions() -> anyhow::Result<()> {
    let state = state_with_accounts(&["acct-a"]).await;
    register(&state, session_entry("s1", Some("acct-a"))).await;

    spawn_throttle_watcher(Arc::clone(&state));

    // An idle transition with no error category and no working state: nothing changes.
    state.feed.event_tx.send(transition("s1", "idle", None))?;
    tokio::time::sleep(Duration::from_millis(120)).await;
    assert_eq!(state.credential_broker.account_status("acct-a").await, "healthy");

    Ok(())
}

// ── handle_throttle: operator kill switch (COOPMUX_DISABLE_FAILOVER) ────────

#[tokio::test]
#[serial]
async fn handle_throttle_kill_switch_skips_rebalance() -> anyhow::Result<()> {
    // With COOPMUX_DISABLE_FAILOVER set, handle_throttle must still mark the
    // account throttled (observability half) but return early WITHOUT running
    // project_scoped_rebalance — even when the account cache DOES have an
    // affected project that would otherwise rebalance. `#[serial]` because the
    // env var is process-global.
    std::env::set_var("COOPMUX_DISABLE_FAILOVER", "1");

    let state = state_with_accounts(&["acct-a", "acct-b"]).await;
    // Seed a cache where "proj" uses acct-a with a fallback to acct-b: absent
    // the kill switch this would rebalance. The early return must beat it.
    seed_rebalanceable_cache(&state, "acct-a", "acct-b", "proj").await;
    register(&state, session_entry("s1", Some("acct-a"))).await;

    let rebalanced = handle_throttle(&state, "s1", "rate_limited", Some("429")).await;

    assert!(!rebalanced, "kill switch must report no rebalance");
    assert_eq!(
        state.credential_broker.account_status("acct-a").await,
        "throttled",
        "throttle marking (observability) still happens under the kill switch"
    );

    std::env::remove_var("COOPMUX_DISABLE_FAILOVER");
    Ok(())
}

// ── handle_throttle: real project-scoped rebalance ─────────────────────────

#[tokio::test]
#[serial]
async fn handle_throttle_rebalances_session_to_project_fallback() -> anyhow::Result<()> {
    // End-to-end over handle_throttle → project_scoped_rebalance →
    // reassign_session: a throttled account whose project has a healthy
    // fallback moves the session onto the fallback account and reports a
    // rebalance. Uses a mock upstream coop that accepts the profile push +
    // switch calls reassign_session makes. `#[serial]` to guard the shared
    // COOPMUX_DISABLE_FAILOVER env against the kill-switch test above.
    std::env::remove_var("COOPMUX_DISABLE_FAILOVER");

    let (upstream_url, switches) = mock_switch_upstream().await?;
    let beads_url = mock_beads_patch().await?;

    let state = state_with_accounts(&["acct-a", "acct-b"]).await;
    seed_rebalanceable_cache(&state, "acct-a", "acct-b", "proj").await;
    // A beads client is required for handle_throttle to REPORT a rebalance:
    // the return map is populated from the config:auth bead update, which
    // no-ops without a client. Session reassignment happens either way.
    *state.beads_client.write().await =
        Some(Arc::new(crate::beads::BeadsClient::new(beads_url, None)?));

    // Session s1 is on the throttled account acct-a and tagged with project
    // "proj" so the project-scoped rebalance picks it up. Its url points at the
    // mock upstream so the reassign switch call succeeds.
    let entry = session_entry_full("s1", Some("acct-a"), upstream_url, Some("proj"));
    register(&state, Arc::clone(&entry)).await;

    let rebalanced = handle_throttle(&state, "s1", "rate_limited", Some("429")).await;

    assert!(rebalanced, "a project with a healthy fallback should report a rebalance");
    assert_eq!(
        state.credential_broker.account_status("acct-a").await,
        "throttled",
        "the throttled account is marked throttled"
    );
    assert_eq!(
        entry.assigned_account.read().await.as_deref(),
        Some("acct-b"),
        "the session should be reassigned to the fallback account"
    );
    assert!(switches.load(Ordering::SeqCst) >= 1, "a profile switch was pushed to the session");
    Ok(())
}

// ── ka-0000004551: rebalance must not hang or wedge the cache on a hung pod ──

/// The reassign HTTP POSTs are timeout-bounded, so a rebalance targeting a hung
/// / parked session pod completes (returns) instead of blocking forever. Uses a
/// paused clock so the (real, 10s) request timeout elapses in virtual time —
/// the test itself finishes near-instantly. Before the fix, `reassign_session`
/// used a timeout-less UpstreamClient on a no-default-timeout shared client, so
/// this would never return.
#[tokio::test(start_paused = true)]
#[serial]
async fn handle_throttle_rebalance_is_bounded_when_upstream_hangs() -> anyhow::Result<()> {
    std::env::remove_var("COOPMUX_DISABLE_FAILOVER");

    let hung_url = mock_hung_upstream().await?;
    let beads_url = mock_beads_patch().await?;

    let state = state_with_accounts(&["acct-a", "acct-b"]).await;
    seed_rebalanceable_cache(&state, "acct-a", "acct-b", "proj").await;
    *state.beads_client.write().await =
        Some(Arc::new(crate::beads::BeadsClient::new(beads_url, None)?));

    // Session s1 is on the throttled account and points at the HUNG upstream.
    let entry = session_entry_full("s1", Some("acct-a"), hung_url, Some("proj"));
    register(&state, Arc::clone(&entry)).await;

    // Wrap in a generous virtual-time budget: with the per-request timeout the
    // two POSTs resolve in ~20s of virtual time; the guard proves boundedness
    // (a pre-fix infinite hang would exhaust this and fail).
    let res = tokio::time::timeout(
        Duration::from_secs(120),
        handle_throttle(&state, "s1", "rate_limited", Some("429")),
    )
    .await;
    assert!(res.is_ok(), "handle_throttle must not hang when the target pod is unresponsive");

    // The account is still marked throttled regardless of the switch outcome —
    // the bookkeeping side does not depend on the (failed) upstream switch.
    assert_eq!(state.credential_broker.account_status("acct-a").await, "throttled");
    Ok(())
}

/// The account_cache read lock is NOT held across the rebalance I/O: while a
/// rebalance to a hung upstream is in flight, a writer can still acquire the
/// cache write lock promptly. Before the fix the read guard was held across
/// `project_scoped_rebalance`, so this writer would block behind the hung POSTs
/// (write-preferring RwLock), wedging the whole credential plane.
#[tokio::test(start_paused = true)]
#[serial]
async fn handle_throttle_does_not_hold_cache_lock_across_rebalance() -> anyhow::Result<()> {
    std::env::remove_var("COOPMUX_DISABLE_FAILOVER");

    let hung_url = mock_hung_upstream().await?;
    let beads_url = mock_beads_patch().await?;

    let state = state_with_accounts(&["acct-a", "acct-b"]).await;
    seed_rebalanceable_cache(&state, "acct-a", "acct-b", "proj").await;
    *state.beads_client.write().await =
        Some(Arc::new(crate::beads::BeadsClient::new(beads_url, None)?));

    let entry = session_entry_full("s1", Some("acct-a"), hung_url, Some("proj"));
    register(&state, Arc::clone(&entry)).await;

    let state2 = Arc::clone(&state);
    let rebalance = tokio::spawn(async move {
        let _ = handle_throttle(&state2, "s1", "rate_limited", Some("429")).await;
    });

    // Give the rebalance a beat to reach its (hung) HTTP loop, then assert a
    // writer can grab the cache write lock without waiting on that I/O.
    tokio::task::yield_now().await;
    let got_lock = tokio::time::timeout(Duration::from_secs(5), state.account_cache.write()).await;
    assert!(
        got_lock.is_ok(),
        "cache write lock must be obtainable while a rebalance to a hung pod is in flight"
    );
    drop(got_lock);

    // Let the bounded rebalance finish so the task does not leak.
    let _ = tokio::time::timeout(Duration::from_secs(120), rebalance).await;
    Ok(())
}
