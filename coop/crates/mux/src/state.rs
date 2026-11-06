// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicU32, AtomicU64};
use std::sync::{Arc, Weak};
use std::time::Instant;

use tokio::sync::{broadcast, Mutex, RwLock};
use tokio_util::sync::CancellationToken;

use crate::config::MuxConfig;
use crate::credential::broker::CredentialBroker;
use crate::credential::failover::AccountCache;
use crate::credential::CredentialEvent;
use crate::transport::sse::SseRegistry;
use crate::upstream::bridge::WsBridge;
use crate::upstream::prewarm::PrewarmCache;

/// Events emitted by the mux for aggregation consumers.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
#[serde(tag = "event", rename_all = "snake_case")]
pub enum MuxEvent {
    /// An agent state transition from an upstream session.
    ///
    /// Fields mirror the upstream single-session `Transition` WebSocket message,
    /// plus `session` to identify which upstream session it came from.
    Transition {
        session: String,
        prev: String,
        next: String,
        seq: u64,
        #[serde(default, skip_serializing_if = "String::is_empty")]
        cause: String,
        #[serde(skip_serializing_if = "Option::is_none")]
        last_message: Option<String>,
        #[serde(skip_serializing_if = "Option::is_none")]
        prompt: Option<serde_json::Value>,
        #[serde(skip_serializing_if = "Option::is_none")]
        error_detail: Option<String>,
        #[serde(skip_serializing_if = "Option::is_none")]
        error_category: Option<String>,
        #[serde(skip_serializing_if = "Option::is_none")]
        parked_reason: Option<String>,
        #[serde(skip_serializing_if = "Option::is_none")]
        resume_at_epoch_ms: Option<u64>,
    },
    /// An upstream session came online (feed connected).
    #[serde(rename = "session:online")]
    SessionOnline { session: String, url: String, metadata: serde_json::Value },
    /// An upstream session went offline (deregistered or feed disconnected).
    #[serde(rename = "session:offline")]
    SessionOffline { session: String },
    /// Credentials refreshed successfully for an account.
    #[serde(rename = "credential:refreshed")]
    CredentialRefreshed { account: String },
    /// A credential refresh attempt failed.
    #[serde(rename = "credential:refresh:failed")]
    CredentialRefreshFailed { account: String, error: String },
    /// Rate-limit probe data updated for an account (from controller ratelimit_probe).
    #[serde(rename = "probe:updated")]
    ProbeUpdated {
        account: String,
        throttle_status: String,
        utilization_5h: String,
        utilization_7d: String,
        throttle_reset_at: String,
    },
}

impl MuxEvent {
    /// Convert a [`CredentialEvent`] into a `MuxEvent`, stripping secrets
    /// (access tokens from `Refreshed` are not included).
    pub fn from_credential(event: &CredentialEvent) -> Self {
        match event {
            CredentialEvent::Refreshed { account, .. } => {
                Self::CredentialRefreshed { account: account.clone() }
            }
            CredentialEvent::RefreshFailed { account, error } => {
                Self::CredentialRefreshFailed { account: account.clone(), error: error.clone() }
            }
        }
    }
}

/// Per-session event feed and watcher tracking.
pub struct SessionFeed {
    /// Broadcast channel for mux events (state transitions, online/offline).
    pub event_tx: broadcast::Sender<MuxEvent>,
    /// Per-session watcher set. Screen/status pollers start when non-empty and
    /// stop when the last watcher leaves.
    ///
    /// HTTP event feeds are owned by the registered [`SessionEntry`] lifetime,
    /// not by dashboard watchers.
    pub watchers: RwLock<HashMap<String, WatcherState>>,
    /// Monotonic source of unique per-feed watcher ids (see [`next_watcher_id`]).
    watcher_id_seq: AtomicU64,
}

/// Identifies a single dashboard feed loop watching a session. Each
/// `run_mux_feed` invocation gets one id at startup and uses it for every
/// session it watches, so a session's watcher population is the *set* of feed
/// ids — not an integer count that a generation swap could collapse.
pub type WatcherId = u64;

/// Tracks the set of dashboard feeds watching a session and the poller
/// cancellation for the current session generation.
///
/// A watcher *set* (rather than an integer refcount) is what makes an
/// offline -> online generation swap safe: `remove_session` drops the whole
/// entry on offline, then every surviving feed re-adds its own id on
/// `SessionOnline`, reconstructing the true population. An integer count reset
/// to 1 on re-watch would collapse N concurrent watchers to 1, letting the
/// first unsubscribe/disconnect tear down the shared poller out from under the
/// others (ka-0000004753).
pub struct WatcherState {
    /// Feed ids currently watching this session generation.
    pub watchers: HashSet<WatcherId>,
    /// Session generation this watcher belongs to.
    pub entry: Weak<SessionEntry>,
    /// Cancel token for screen + status pollers.
    pub poller_cancel: CancellationToken,
}

impl WatcherState {
    /// Number of distinct feeds currently watching this session generation.
    pub fn count(&self) -> usize {
        self.watchers.len()
    }
}

impl Default for SessionFeed {
    fn default() -> Self {
        Self::new()
    }
}

impl SessionFeed {
    pub fn new() -> Self {
        let (event_tx, _) = broadcast::channel(512);
        Self { event_tx, watchers: RwLock::new(HashMap::new()), watcher_id_seq: AtomicU64::new(0) }
    }

    /// Allocate a fresh, process-unique id for a dashboard feed loop.
    pub fn next_watcher_id(&self) -> WatcherId {
        self.watcher_id_seq.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
    }
}

/// Shared mux state.
pub struct MuxState {
    pub sessions: RwLock<HashMap<String, Arc<SessionEntry>>>,
    pub config: MuxConfig,
    pub shutdown: CancellationToken,
    pub feed: SessionFeed,
    pub credential_broker: Arc<CredentialBroker>,
    pub prewarm: Arc<Mutex<PrewarmCache>>,
    /// Shared HTTP client for all upstream coop traffic.
    ///
    /// Owns the connection pool, idle-keepalive task, DNS resolver, and TLS
    /// context. `reqwest::Client` is cheap to clone (Arc internally), so
    /// per-call sites should `state.http_client.clone()` rather than rebuild
    /// a `Client::builder()`. See ka-0000001805 for the leak history.
    pub http_client: reqwest::Client,
    /// NATS client for publishing input commands to NATS-transport sessions.
    /// Set when a NATS relay subscriber is configured.
    pub nats_client: RwLock<Option<async_nats::Client>>,
    /// Cached view of account beads and auth configs from the beads daemon.
    /// Used for project-scoped failover when an account is throttled.
    pub account_cache: RwLock<AccountCache>,
    /// Beads daemon client for reading/updating beads.
    /// Set during startup if the beads daemon is configured.
    pub beads_client: RwLock<Option<Arc<crate::beads::BeadsClient>>>,
    /// Beads daemon client authenticating as coopmux's own machine principal
    /// (gd-Wcu0DOkHfN). Set at startup when the BEADS_OIDC_* env is configured;
    /// used for every beads read (account cache refresh, claimed-bead
    /// enricher) and the account-bead write in `credential::failover`. `None`
    /// means those go through `beads_client` (the static token), as before.
    /// coopmux does not write `config:auth`; the controller owns that record.
    pub machine_beads_client: Option<Arc<crate::beads::BeadsClient>>,
    /// OIDC JWT verifier. When set, JWT bearer tokens are accepted as an
    /// alternative to the static auth token.
    pub oidc_verifier: Option<Arc<crate::transport::oidc::OidcVerifier>>,
    /// Land-token key (gd-zVTBvaSIdG). When set, a land token is accepted as
    /// a bearer bound to its one land; see `transport::land_principal`.
    pub land_token_key: Option<Arc<crate::transport::land_principal::LandTokenKey>>,
    /// Edge-auth HMAC verification config (ka-0000007914 / W-B-11).
    /// Loaded from env via `Config::from_env()` at startup.
    pub edge_auth_cfg: crate::transport::edge_auth::Config,
    /// Counters for edge-auth verify outcomes; rendered on `/metrics`.
    pub edge_auth_metrics: Arc<crate::transport::edge_auth::EdgeAuthMetrics>,
    /// Registry of live SSE fallback connections (used behind AWS Verified
    /// Access, which drops WebSocket upgrades). Maps a client-generated
    /// connection id to its upstream control sink. See `transport::sse`.
    pub sse: Arc<SseRegistry>,
}

impl MuxState {
    pub fn new(
        config: MuxConfig,
        shutdown: CancellationToken,
        broker: Arc<CredentialBroker>,
    ) -> Self {
        let prewarm = Arc::new(Mutex::new(PrewarmCache::new(config.prewarm_capacity)));
        // Build a single shared `reqwest::Client` for the whole process.
        // `unwrap_or_default` matches the previous per-call behavior.
        let http_client =
            reqwest::Client::builder().pool_max_idle_per_host(32).build().unwrap_or_default();
        Self {
            sessions: RwLock::new(HashMap::new()),
            prewarm,
            config,
            shutdown,
            feed: SessionFeed::new(),
            credential_broker: broker,
            http_client,
            nats_client: RwLock::new(None),
            account_cache: RwLock::new(AccountCache::new()),
            beads_client: RwLock::new(None),
            machine_beads_client: None,
            oidc_verifier: None,
            land_token_key: None,
            edge_auth_cfg: crate::transport::edge_auth::Config::default(),
            edge_auth_metrics: Arc::new(crate::transport::edge_auth::EdgeAuthMetrics::new()),
            sse: Arc::new(SseRegistry::new()),
        }
    }

    /// Remove a session and clean up all associated resources.
    ///
    /// Cancels the session entry, emits `SessionOffline`, and tears down
    /// any active watcher poller tasks.
    pub async fn remove_session(&self, id: &str) -> Option<Arc<SessionEntry>> {
        let entry = self.sessions.write().await.remove(id)?;
        entry.cancel.cancel();
        let _ = self.feed.event_tx.send(MuxEvent::SessionOffline { session: id.to_owned() });
        let mut watchers = self.feed.watchers.write().await;
        let removes_current_generation = watchers
            .get(id)
            .and_then(|watcher| watcher.entry.upgrade())
            .is_some_and(|watched_entry| Arc::ptr_eq(&watched_entry, &entry));
        if removes_current_generation {
            if let Some(ws) = watchers.remove(id) {
                ws.poller_cancel.cancel();
            }
        }
        drop(watchers);
        self.prewarm.lock().await.remove(id);
        Some(entry)
    }
}

/// How coopmux communicates with an upstream coop session.
#[derive(Debug, Clone, Default)]
pub enum SessionTransport {
    /// Direct HTTP to the coop instance (default for K8s sessions).
    #[default]
    Http,
    /// NATS relay: session discovered via NATS announce, input routed via NATS.
    Nats { prefix: String },
}

/// A registered upstream coop session.
pub struct SessionEntry {
    pub id: String,
    pub url: String,
    pub auth_token: Option<String>,
    pub metadata: serde_json::Value,
    pub registered_at: Instant,
    pub cached_screen: RwLock<Option<CachedScreen>>,
    pub cached_status: RwLock<Option<CachedStatus>>,
    pub cached_usage: RwLock<Option<CachedUsage>>,
    pub health_failures: AtomicU32,
    pub cancel: CancellationToken,
    pub ws_bridge: RwLock<Option<Arc<WsBridge>>>,
    /// The credential account assigned to this session by the pool.
    /// Set during registration, used for unassignment on removal.
    pub assigned_account: RwLock<Option<String>>,
    /// Title of the bead this agent is currently working on.
    /// Populated by periodic enrichment from the beads daemon.
    pub claimed_bead_title: RwLock<Option<String>>,
    /// Transport type for this session. NATS-transport sessions are discovered
    /// via NATS announce and have input routed through NATS instead of HTTP.
    pub transport: SessionTransport,
}

/// Cached screen snapshot from upstream.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct CachedScreen {
    pub lines: Vec<String>,
    pub ansi: Vec<String>,
    pub cols: u16,
    pub rows: u16,
    pub alt_screen: bool,
    pub seq: u64,
    pub fetched_at: u64,
}

/// Cached usage from upstream (cost + token counts).
///
/// Every field is `#[serde(default)]` because the NATS relay producer
/// (`coop` `usage_loop`) emits only the token/request counters and never
/// `total_cost_usd` or `fetched_at`; a strict decode would reject every usage
/// message on the NATS transport. The HTTP poller builds this struct
/// field-by-field (see `poller.rs`), so the defaults never affect that path.
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize)]
pub struct CachedUsage {
    #[serde(default)]
    pub input_tokens: u64,
    #[serde(default)]
    pub output_tokens: u64,
    #[serde(default)]
    pub total_cost_usd: f64,
    #[serde(default)]
    pub request_count: u64,
    #[serde(default)]
    pub fetched_at: u64,
}

/// Cached status from upstream.
///
/// Every field is `#[serde(default)]` for the same reason as `CachedUsage`:
/// the NATS relay producer serializes a `SessionStatus` that has no
/// `fetched_at`, so a strict decode would reject every status message on the
/// NATS transport. `fetched_at` is stamped at receive time by the handler.
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize)]
pub struct CachedStatus {
    #[serde(default)]
    pub session_id: String,
    #[serde(default)]
    pub state: String,
    #[serde(default)]
    pub pid: Option<i32>,
    #[serde(default)]
    pub uptime_secs: i64,
    #[serde(default)]
    pub exit_code: Option<i32>,
    #[serde(default)]
    pub screen_seq: u64,
    #[serde(default)]
    pub bytes_read: u64,
    #[serde(default)]
    pub bytes_written: u64,
    #[serde(default)]
    pub ws_clients: i32,
    #[serde(default)]
    pub fetched_at: u64,
}

/// Return current epoch millis.
pub fn epoch_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}
