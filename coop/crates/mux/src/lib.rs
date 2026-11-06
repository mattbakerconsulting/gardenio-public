// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Coopmux: PTY multiplexing proxy for coop instances.

pub mod beads;
pub mod config;
pub mod credential;
pub mod error;
pub mod machine_token;
pub mod state;
pub mod transport;
pub mod upstream;

use std::sync::Arc;

use tokio::net::TcpListener;
use tokio::sync::broadcast;
use tokio_util::sync::CancellationToken;

use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::CredentialConfig;
use crate::state::MuxState;
#[cfg(not(debug_assertions))]
use crate::transport::build_router;
#[cfg(debug_assertions)]
use crate::transport::build_router_hot;
use crate::upstream::health::spawn_health_checker;
use crate::upstream::prewarm::spawn_prewarm_task;

/// Optional NATS event publishing configuration.
///
/// Passed separately from [`MuxConfig`] because these args live on the
/// binary's CLI struct rather than the library config.
pub struct NatsConfig {
    pub url: String,
    pub token: Option<String>,
    /// NATS username. When set, `token` is sent as this user's PASSWORD —
    /// multi-user server auth (nats.auth.subjectIsolation) only supports
    /// user/password entries (ka-0000000002).
    pub user: Option<String>,
    pub prefix: String,
}

/// Optional NATS relay configuration for auto-discovering local agent sessions.
pub use crate::transport::nats_sub::NatsRelayConfig;

/// Run the mux server until shutdown.
pub async fn run(
    config: MuxConfig,
    nats: Option<NatsConfig>,
    nats_relay: Option<NatsRelayConfig>,
) -> anyhow::Result<()> {
    let addr = format!("{}:{}", config.host, config.port);
    let shutdown = CancellationToken::new();

    // Always initialize credential broker (empty config if no file provided).
    // Credential config file is deprecated — accounts should come from the
    // controller via the credentials/set API at runtime.
    let cred_config = match config.credential_config {
        Some(ref cred_path) => {
            tracing::warn!(
                path = %cred_path.display(),
                "--credential-config is deprecated and will be removed in a future release. \
                 Credentials should be synced from the controller via the credentials/set API."
            );
            let contents = std::fs::read_to_string(cred_path)?;
            serde_json::from_str::<CredentialConfig>(&contents)?
        }
        None => CredentialConfig { accounts: vec![] },
    };

    let (event_tx, event_rx) = broadcast::channel(64);
    let cred_bridge_rx = event_tx.subscribe();

    let state_dir = config.state_dir();
    let broker = CredentialBroker::new(cred_config, event_tx, Some(state_dir.clone()));

    // Fail-closed: require a credential encryption key (or the explicit
    // COOP_ALLOW_PLAINTEXT_BOOTSTRAP escape hatch) before doing anything with
    // credentials, even on a fresh install with no persist file.
    crate::credential::persist::validate_key_policy()?;

    // Load persisted credentials (including dynamic accounts) if available.
    let persist_path = state_dir.join("credentials.json");
    if persist_path.exists() {
        let persisted = crate::credential::persist::load(&persist_path)?;
        broker.load_persisted(&persisted).await;
    }

    // Seed API key from file if configured (static key mode for K8s deployments).
    if let Some(ref key_file) = config.api_key_file {
        match std::fs::read_to_string(key_file) {
            Ok(contents) => {
                let api_key = contents.trim().to_owned();
                if api_key.is_empty() {
                    tracing::warn!(path = %key_file.display(), "api-key-file is empty, skipping seed");
                } else if let Some(account) = broker.first_account_name().await {
                    match broker.set_token(&account, api_key, None, None).await {
                        Ok(()) => {
                            tracing::info!(account = %account, path = %key_file.display(), "seeded API key from file")
                        }
                        Err(e) => {
                            tracing::error!(account = %account, err = %e, "failed to seed API key from file")
                        }
                    }
                } else {
                    tracing::warn!("api-key-file provided but no accounts configured");
                }
            }
            Err(e) => {
                tracing::error!(path = %key_file.display(), err = %e, "failed to read api-key-file")
            }
        }
    }

    let mut state = MuxState::new(config.clone(), shutdown.clone(), Arc::clone(&broker));

    // Edge-auth HMAC verify config (ka-0000007914 / W-B-11). Burn-in by
    // default: when DAEMON_HMAC_KEY is set we count + log non-OK outcomes
    // but still honour X-Operator. Flipping enforce=true is gated on a
    // clean ≥7d soak per env (runbook ka-0000000026). With NO key there is
    // nothing to verify against, so X-Operator is refused outright (never
    // burn-in trusted): header identity only exists where edge-auth signs it.
    {
        let cfg = crate::transport::edge_auth::Config::from_env();
        if cfg.enabled() {
            tracing::info!(enforce = cfg.enforce, "edge-auth HMAC verify enabled");
        } else {
            tracing::debug!("edge-auth HMAC verify disabled (no DAEMON_HMAC_KEY)");
        }
        state.edge_auth_cfg = cfg;
    }

    // Initialize OIDC verifier if configured.
    if let (Some(ref issuer_url), Some(ref client_id)) =
        (&config.oidc_issuer_url, &config.oidc_client_id)
    {
        match crate::transport::oidc::OidcVerifier::new(issuer_url, client_id).await {
            Ok(verifier) => {
                tracing::info!(issuer = %issuer_url, "OIDC auth enabled for coopmux");
                state.oidc_verifier = Some(Arc::new(verifier));
            }
            Err(e) => {
                tracing::error!(err = %e, "failed to initialize OIDC verifier — OIDC auth disabled");
            }
        }
    }

    // Land principal (gd-zVTBvaSIdG): tenant pods authenticate with a land
    // token, so a registration is bound to the land the token names. An
    // unreadable or empty key file refuses to start.
    if let Some(key) = crate::transport::land_principal::LandTokenKey::from_env()? {
        state.land_token_key = Some(Arc::new(key));
        tracing::info!("land tokens enabled");
    }

    // Machine principal (gd-Wcu0DOkHfN, design
    // gardenio/docs/machine-principal-design.md): when BEADS_OIDC_TOKEN_URL,
    // BEADS_OIDC_CLIENT_ID and BEADS_OIDC_CLIENT_SECRET_FILE are all set, the
    // account-bead write in credential/failover.rs (status=exhausted) runs as
    // the Keycloak service account of that client. The beads reads (accounts,
    // auth configs, role accounts, agent/task beads) use it too — see
    // `read_client`. coopmux does not write the config:auth bead; the
    // controller records the failover (gd-HYilfX1Bk8). A
    // half-configured principal is a misconfiguration and refuses to start,
    // same as the controller; a missing secret FILE is not (the Secret mount is
    // optional): the write then fails closed with auth_unavailable in the log.
    match crate::machine_token::MachineTokenSource::from_env()? {
        Some(source) => {
            let client_id = source.client_id().to_owned();
            let client = crate::beads::BeadsClient::new_machine(
                crate::beads::http_addr_from_env(),
                Arc::new(source),
            )?;
            state.machine_beads_client = Some(Arc::new(client));
            tracing::info!(client = %client_id, "machine principal enabled");
        }
        None => tracing::info!("machine principal disabled"),
    }

    // Spawn distributor (pushes credentials to sessions on events).
    let state = Arc::new(state);
    crate::credential::distributor::spawn_distributor(Arc::clone(&state), event_rx);
    let _ = nats;

    // Bridge credential events into the MuxEvent broadcast channel.
    {
        let mux_event_tx = state.feed.event_tx.clone();
        tokio::spawn(async move {
            let mut rx = cred_bridge_rx;
            loop {
                match rx.recv().await {
                    Ok(e) => {
                        let _ = mux_event_tx.send(crate::state::MuxEvent::from_credential(&e));
                    }
                    Err(broadcast::error::RecvError::Lagged(_)) => continue,
                    Err(_) => break,
                }
            }
        });
    }

    // Spawn throttle watcher (monitors session transitions for rate-limit signals).
    crate::credential::throttle::spawn_throttle_watcher(Arc::clone(&state));

    // Initialize the beads daemon client for project-scoped failover.
    // Optional: only active when BEADS_HTTP_ADDR is set or the default is reachable.
    spawn_account_cache_refresh(Arc::clone(&state), shutdown.clone());

    // Spawn the claimed bead title enricher (updates session titles from beads daemon).
    spawn_claimed_bead_enricher(Arc::clone(&state), shutdown.clone());

    let has_creds = config.credential_config.is_some();
    if has_creds {
        tracing::info!("coopmux listening on {addr} (credentials enabled)");
    } else {
        tracing::info!("coopmux listening on {addr}");
    }
    spawn_health_checker(Arc::clone(&state));

    // Spawn NATS relay subscriber for auto-discovering local agent sessions.
    if let Some(relay_config) = nats_relay {
        crate::transport::nats_sub::spawn_nats_subscriber(Arc::clone(&state), relay_config);
    }

    spawn_prewarm_task(
        Arc::clone(&state),
        Arc::clone(&state.prewarm),
        config.prewarm_poll_interval(),
        shutdown.clone(),
    );
    #[cfg(debug_assertions)]
    let router = build_router_hot(state, config.hot);
    #[cfg(not(debug_assertions))]
    let router = build_router(state);
    let listener = TcpListener::bind(&addr).await?;
    axum::serve(listener, router).with_graceful_shutdown(shutdown.cancelled_owned()).await?;

    Ok(())
}

/// The client for beads READS: coopmux's machine principal when configured
/// (gardenio-operator, which every machine client holds, covers reads), else
/// the static-token `fallback`.
fn read_client(
    state: &MuxState,
    fallback: Arc<crate::beads::BeadsClient>,
) -> Arc<crate::beads::BeadsClient> {
    state.machine_beads_client.clone().unwrap_or(fallback)
}

/// Spawn a background task that periodically enriches sessions with the title
/// of the bead each agent is currently working on. Queries the beads daemon
/// for agent beads → task_id → task title, caching the result on each
/// [`SessionEntry`].
fn spawn_claimed_bead_enricher(state: Arc<MuxState>, shutdown: CancellationToken) {
    tokio::spawn(async move {
        // Wait for the beads client to become available (set by account_cache_refresh).
        let client = loop {
            {
                let guard = state.beads_client.read().await;
                if let Some(ref c) = *guard {
                    break Arc::clone(c);
                }
            }
            tokio::select! {
                _ = tokio::time::sleep(std::time::Duration::from_secs(5)) => {}
                _ = shutdown.cancelled() => return,
            }
        };

        let client = read_client(&state, client);
        let enrich_interval = std::time::Duration::from_secs(30);
        loop {
            enrich_claimed_titles(&state, &client).await;

            tokio::select! {
                _ = tokio::time::sleep(enrich_interval) => {}
                _ = shutdown.cancelled() => break,
            }
        }
    });
}

/// One pass of claimed-bead enrichment: for each session with an
/// `agent_bead_id` in its metadata, look up the agent's `task_id` field,
/// then fetch the task bead's title and cache it on the session entry.
async fn enrich_claimed_titles(state: &MuxState, client: &crate::beads::BeadsClient) {
    // Collect agent_bead_ids from all sessions.
    let sessions = state.sessions.read().await;
    let mut agent_to_session: Vec<(String, String)> = Vec::new(); // (agent_bead_id, session_id)
    for entry in sessions.values() {
        if let Some(bead_id) = entry.metadata.get("agent_bead_id").and_then(|v| v.as_str()) {
            if !bead_id.is_empty() {
                agent_to_session.push((bead_id.to_owned(), entry.id.clone()));
            }
        }
    }
    drop(sessions);

    if agent_to_session.is_empty() {
        return;
    }

    // Batch-fetch all agent beads to get their task_id fields.
    let agent_ids: Vec<&str> = agent_to_session.iter().map(|(id, _)| id.as_str()).collect();
    let agent_beads = match client.get_beads_batch(&agent_ids).await {
        Ok(m) => m,
        Err(e) => {
            tracing::debug!(err = %e, "failed to fetch agent beads for enrichment");
            return;
        }
    };

    // Collect task IDs from agent beads.
    let mut task_ids: Vec<String> = Vec::new();
    let mut agent_task_map: std::collections::HashMap<String, String> =
        std::collections::HashMap::new(); // agent_bead_id → task_id
    for (agent_id, _session_id) in &agent_to_session {
        if let Some(bead) = agent_beads.get(agent_id) {
            if let Some(task_id) = bead.fields.get("task_id").filter(|s| !s.is_empty()) {
                task_ids.push(task_id.clone());
                agent_task_map.insert(agent_id.clone(), task_id.clone());
            }
        }
    }

    if task_ids.is_empty() {
        return;
    }

    // Batch-fetch all task beads to get their titles.
    let task_id_refs: Vec<&str> = task_ids.iter().map(|s| s.as_str()).collect();
    let task_beads = match client.get_beads_batch(&task_id_refs).await {
        Ok(m) => m,
        Err(e) => {
            tracing::debug!(err = %e, "failed to fetch task beads for enrichment");
            return;
        }
    };

    // Update session entries with claimed bead titles.
    let sessions = state.sessions.read().await;
    for (agent_id, session_id) in &agent_to_session {
        let title = agent_task_map
            .get(agent_id)
            .and_then(|task_id| task_beads.get(task_id))
            .map(|bead| bead.title.clone());
        if let Some(entry) = sessions.get(session_id) {
            *entry.claimed_bead_title.write().await = title;
        }
    }
}

/// Spawn a background task that periodically refreshes the account cache
/// from the beads daemon. This enables project-scoped failover when an
/// account is throttled.
fn spawn_account_cache_refresh(state: Arc<MuxState>, shutdown: CancellationToken) {
    tokio::spawn(async move {
        // Try to initialize the beads client. If it fails (e.g. no TLS provider),
        // log and exit — failover will use the generic path instead.
        let client = match crate::beads::BeadsClient::from_env() {
            Ok(c) => Arc::new(c),
            Err(e) => {
                tracing::info!(err = %e, "beads client not available, project-scoped failover disabled");
                return;
            }
        };

        // Store the client in state for use by other components.
        *state.beads_client.write().await = Some(Arc::clone(&client));

        // Reads go through the machine principal when configured; `client`
        // (static token) stays in state as the fallback when it is not.
        let client = read_client(&state, client);

        let refresh_interval = std::time::Duration::from_secs(60);
        let mut first = true;
        // Track last-seen probe state to detect changes and emit WS events.
        let mut last_probe: std::collections::HashMap<String, (String, String, String)> =
            std::collections::HashMap::new();
        // Track last-seen bead-authoritative usability per account so we can
        // re-push the health flag to sessions when it flips (ka-0000006430).
        // A profile staged while its account was healthy is never de-staged, so
        // without this a session that later 429s could rotate onto it after it
        // went exhausted (the fo85 incident). On the FIRST refresh this map is
        // empty, so no re-push fires — staging already stamped the initial flag.
        let mut last_usable: std::collections::HashMap<String, bool> =
            std::collections::HashMap::new();

        loop {
            // Accounts whose usability flipped this cycle; the health flag is
            // re-pushed to sessions AFTER the cache lock is released (below),
            // because distribute_to_sessions also reads the account cache and
            // would otherwise deadlock against the write lock held here.
            let mut usability_flips: Vec<(String, bool)> = Vec::new();
            // Fetch the refreshed cache OFF-LOCK: the three sequential beads
            // RPCs (each up to the client's 30s timeout) must never run while
            // the account-cache write guard is held, or every credential-plane
            // reader — new-session staging, distributor stamps, throttle
            // handling, operator pool endpoints — starves for the whole window
            // (ka-0000000143; same class as ka-0000004551). We build the fresh
            // cache with no lock held, then swap it in under a brief write lock.
            match crate::credential::failover::AccountCache::fetch(&*client).await {
                Ok(fresh) => {
                    // Swap the freshly-built cache in under a brief write lock,
                    // then read back the ProbeUpdated / usability-flip signals
                    // while still holding it (these are cheap in-memory scans).
                    let mut cache = state.account_cache.write().await;
                    *cache = fresh;

                    let is_first = first;
                    if first {
                        tracing::info!("account cache initialized from beads daemon");
                        first = false;
                    }

                    // Emit ProbeUpdated WS events for accounts whose probe data changed.
                    for (name, info) in cache.all_accounts() {
                        let key = (
                            info.throttle_status.clone(),
                            info.utilization_5h.clone(),
                            info.utilization_7d.clone(),
                        );
                        let prev = last_probe.get(name);
                        if prev.map(|p| p != &key).unwrap_or(!key.0.is_empty()) {
                            let _ =
                                state.feed.event_tx.send(crate::state::MuxEvent::ProbeUpdated {
                                    account: name.clone(),
                                    throttle_status: info.throttle_status.clone(),
                                    utilization_5h: info.utilization_5h.clone(),
                                    utilization_7d: info.utilization_7d.clone(),
                                    throttle_reset_at: info.throttle_reset_at.clone(),
                                });
                            last_probe.insert(name.clone(), key);
                        }
                    }

                    // Detect usability flips. Seed the map on the first
                    // refresh WITHOUT emitting flips (staging stamped the
                    // initial flag); afterwards, any account whose usability
                    // changed is queued for a health-flag re-push.
                    for name in cache.all_accounts().keys() {
                        let now_usable = cache.account_usable(name);
                        match last_usable.get(name) {
                            Some(&prev) if prev != now_usable => {
                                usability_flips.push((name.clone(), now_usable));
                            }
                            _ => {}
                        }
                        last_usable.insert(name.clone(), now_usable);
                    }
                    if is_first {
                        // First refresh only seeds the baseline.
                        usability_flips.clear();
                    }
                }
                Err(e) => {
                    if first {
                        tracing::info!(err = %e, "beads daemon not reachable, will retry");
                        first = false;
                    } else {
                        tracing::debug!(err = %e, "account cache refresh failed");
                    }
                }
            }

            // Re-push the current health flag for each flipped account (lock
            // released). distribute_to_sessions with switch=false re-registers
            // the profile — an idempotent upsert that only refreshes creds and
            // stamps the new `usable` flag — so coop's try_auto_rotate stops
            // (or resumes) treating the profile as a rotation candidate. It
            // recomputes the health flag itself, so we pass switch=false only.
            for (account, now_usable) in usability_flips {
                let Some(credentials) = state.credential_broker.get_credentials(&account).await
                else {
                    tracing::debug!(
                        account = %account,
                        "health re-push: no credentials for flipped account, skipping"
                    );
                    continue;
                };
                tracing::info!(
                    account = %account,
                    usable = now_usable,
                    "health re-push: account usability flipped, re-pushing health flag to sessions"
                );
                crate::credential::distributor::distribute_to_sessions(
                    &state,
                    &account,
                    &credentials,
                    false,
                )
                .await;
            }

            tokio::select! {
                _ = tokio::time::sleep(refresh_interval) => {}
                _ = shutdown.cancelled() => break,
            }
        }
    });
}

#[cfg(test)]
#[path = "lib_tests.rs"]
mod tests;
