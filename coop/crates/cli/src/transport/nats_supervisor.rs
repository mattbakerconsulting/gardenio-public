// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! NATS transport supervisor — one place that owns connecting (and *re*-connecting)
//! coop's four NATS transports.
//!
//! Stages 3–4 of the agent rehydration design
//! (`gardenio/docs/gvisor-runsc-cr/rehydration-design.md`, ka-0000001515; build
//! bead ka-0000001901). When a checkpointed gVisor agent is thawed into a fresh
//! kubelet-owned pod, the process never restarts, so the four async-nats clients
//! coop spawned at boot still hold TCP sockets bound to the *old* pod's 4-tuple —
//! dead on first write after the IP change (N1–N4, rehydration-design §2.2). The
//! design calls for an **explicit, deterministic teardown + fresh `connect()`**
//! with a freshly-resolved token, not a reliance on async-nats noticing a broken
//! pipe and reconnecting with the *stale* token it captured at the original
//! `connect()` (rehydration-design §2.2 "Why async-nats auto-reconnect is *not*
//! sufficient").
//!
//! Both boot and thaw share one path: `connect_all` spawns the transports under a
//! per-generation child of the parent shutdown token; `reconnect_all` cancels the
//! previous generation (dropping the dead clients cleanly via `async_nats`'s
//! `Drop`) and re-spawns a fresh generation with a re-resolved token.
//!
//! The relay (N2) re-announces presence eagerly on every (re)connect — its
//! `announce_loop` publishes an `online` event before its first heartbeat tick —
//! so a fresh generation re-registers the session with coopmux without waiting on
//! the 30 s timer (C4, rehydration-design §4 Stage 4).

use std::net::IpAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;

use crate::transport::nats::{NatsAuth, NatsPublisher};
use crate::transport::nats_forward::NatsForwarder;
use crate::transport::nats_relay::{NatsRelay, NatsRelaySubscriber};
use crate::transport::nats_url::{host_of_url, rewrite_host_to_ip};
use crate::transport::state::Store;

/// Fraction of the marker deadline the thaw reconnect may spend retrying the
/// *required* transports' `connect()` (bug ka-0000001492). A brand-new pod's
/// CNI netns may not have working egress to kube-dns for the first few seconds
/// after a runsc restore, so the restored sentry's first reconnect resolves the
/// bare NATS service name and gets `EAI_AGAIN` ("Temporary failure in name
/// resolution") — a *transient* miss, not a missing host. The design
/// (`rehydration-design.md` §4 Stage 3) prescribes a bounded retry under the
/// Stage-5 deadline; without it a single cold-netns miss fail-closes the thaw
/// and burns the whole budget. We cap retries below the full deadline so the
/// ack still writes before `run_handler`'s outer `timeout(marker.deadline())`.
const RECONNECT_RETRY_FRACTION: f32 = 0.85;

/// Backoff floor/ceiling for the required-transport reconnect retry. Small so
/// we re-probe kube-dns reachability frequently as the netns warms up, capped
/// so we never sleep through a large slice of the deadline.
const RETRY_BACKOFF_START: Duration = Duration::from_millis(100);
const RETRY_BACKOFF_MAX: Duration = Duration::from_millis(1000);

/// Per-attempt connection-establishment cap for the *thaw* reconnect only (bug
/// ka-0000001492). async-nats's default `connection_timeout` is 5 s and governs
/// the *whole* handshake (TCP/TLS + server INFO + CONNECT/PONG). On a freshly
/// grafted post-restore CNI netns, kube-proxy may not have programmed the DNAT
/// rule for the NATS ClusterIP yet, so a TCP SYN to that VIP is black-holed and
/// each attempt blocks the full 5 s before erroring. That defeats the fast
/// backoff above — the loop only re-probes every ~5 s, yielding ~8 probes in the
/// retry window instead of the dozens the 100 ms–1 s backoff implies. Capping a
/// single thaw attempt's handshake short makes each failed dial return fast, so
/// the bounded retry re-probes often and lands the instant the route is
/// programmed. The boot path keeps the 5 s default (network is warm; we must not
/// change boot semantics). Overridable via `COOP_THAW_NATS_CONNECT_TIMEOUT_MS`.
const THAW_CONNECT_TIMEOUT: Duration = Duration::from_millis(2000);

/// Env override for [`THAW_CONNECT_TIMEOUT`]. A `0` or unparseable value falls
/// back to the default.
const THAW_CONNECT_TIMEOUT_ENV: &str = "COOP_THAW_NATS_CONNECT_TIMEOUT_MS";

/// On the thaw reconnect, re-resolve the bare NATS host *live* on every Nth
/// retry attempt instead of dialing the boot-cached IP literal (bug
/// ka-0000001492). The boot prewarm (`prewarm_dns`) caches the ClusterIP once
/// while DNS is warm so the cold-netns reconnect can dial an IP literal with
/// zero DNS — but that cache is a *single point of failure*: if the boot
/// resolve never ran, ran in a degraded state, or cached a value that no longer
/// has a route, every retry dials the same dead literal and fail-closes the
/// whole deadline with `ENETUNREACH` even though a fresh `getaddrinfo` would
/// return a reachable address (observed on canary eb1: coop `ENETUNREACH` for
/// the entire window while gb's heartbeat succeeded on the *same* Pod B netns,
/// proving egress + DNS were live). Interleaving a live re-resolve lets a stale
/// or missing cache self-heal within a few attempts. The fast cached-IP path is
/// still used on attempt 0 and most attempts, so the happy path (correct cache
/// + warm route) is unchanged.
const RERESOLVE_EVERY: u32 = 3;

/// Bounded timeout for the live bare-host re-resolve on the thaw path. Kept well
/// under the deadline so a hung `getaddrinfo` on a still-cold netns can't eat
/// the budget; on timeout we fall back to the cached IP / bare name.
const RERESOLVE_TIMEOUT: Duration = Duration::from_millis(1500);

/// Resolve the per-attempt thaw connect timeout, honoring the env override.
fn thaw_connect_timeout() -> Duration {
    parse_thaw_connect_timeout(std::env::var(THAW_CONNECT_TIMEOUT_ENV).ok())
}

/// Pure parse of the thaw-connect-timeout override (split out for testing,
/// since the env-reading wrapper is process-global and racy under parallel
/// tests). A positive integer of milliseconds wins; `0`, empty, absent, or
/// unparseable falls back to the [`THAW_CONNECT_TIMEOUT`] default.
fn parse_thaw_connect_timeout(raw: Option<String>) -> Duration {
    match raw.and_then(|v| v.trim().parse::<u64>().ok()) {
        Some(ms) if ms > 0 => Duration::from_millis(ms),
        _ => THAW_CONNECT_TIMEOUT,
    }
}

/// Retry a fallible async `connect()` until it succeeds or `deadline` passes,
/// with capped exponential backoff. `deadline = None` means single-shot (the
/// boot path, where the network is already warm — we must not slow `connect_all`
/// or change boot semantics). On timeout the last error is returned so the
/// caller still records a `Failed` stage. Each attempt re-invokes the closure
/// with the zero-based attempt index, so the auth token is re-resolved per
/// attempt (a kubelet token refresh mid-retry is picked up) and the dial URL can
/// vary per attempt (the cached-IP fast path vs a periodic live re-resolve, bug
/// ka-0000001492).
async fn connect_with_retry<T, F, Fut>(
    deadline: Option<tokio::time::Instant>,
    label: &str,
    mut connect: F,
) -> anyhow::Result<T>
where
    F: FnMut(u32) -> Fut,
    Fut: std::future::Future<Output = anyhow::Result<T>>,
{
    let mut backoff = RETRY_BACKOFF_START;
    let mut attempt: u32 = 0;
    loop {
        match connect(attempt).await {
            Ok(v) => return Ok(v),
            Err(e) => {
                // Boot path: no deadline → single-shot, preserve prior behaviour.
                let Some(dl) = deadline else { return Err(e) };
                let now = tokio::time::Instant::now();
                if now >= dl {
                    return Err(e);
                }
                // Never sleep past the deadline.
                let sleep = backoff.min(dl - now);
                tracing::warn!(
                    transport = label,
                    err = %e,
                    attempt,
                    retry_in_ms = sleep.as_millis() as u64,
                    "nats-supervisor: connect failed (transient?), retrying under thaw deadline"
                );
                tokio::time::sleep(sleep).await;
                backoff = (backoff * 2).min(RETRY_BACKOFF_MAX);
                attempt = attempt.saturating_add(1);
            }
        }
    }
}

/// Connection parameters for the four transports, captured from `Config` at
/// supervisor construction. The auth token is re-resolved on every (re)connect
/// so a thaw picks up a kubelet-fresh token (rehydration-design C2/C3).
#[derive(Debug, Clone)]
pub struct NatsParams {
    /// Endpoint coop dials.
    pub url: String,
    /// Subject prefix for the general event publisher (N1) and message
    /// forwarder (N3): `{prefix}.state`, `{prefix}.messages.*`,
    /// `{prefix}.account.exhausted`, etc. Default "coop.events".
    pub prefix: String,
    /// Subject prefix for the coopmux discovery relay (N2):
    /// `{relay_prefix}.session.{id}.*`. Kept DISTINCT from `prefix` so relay
    /// session traffic does not collide with the general `coop.events.>` space
    /// and stays within the agent's `coop.mux.session.>` publish grant
    /// (ka-0000005381). Default "coop.mux".
    pub relay_prefix: String,
    /// Agent type string (e.g. "claude"), used for session metadata.
    pub agent: String,
    pub labels: Vec<String>,

    /// Spawn the event publisher (N1). Always on when a URL is configured.
    pub publisher: bool,
    /// Spawn the relay publisher + subscriber (N2) for coopmux discovery.
    pub relay: bool,
    /// Spawn the message forwarder (N3).
    pub forward: bool,
    /// Inbox JetStream consumer agent name (N4); `None` disables the inbox.
    pub inbox_agent: Option<String>,
    pub inbox_rig: Option<String>,
    pub inject_dir: PathBuf,

    // -- auth (re-read fresh on each connect) --
    /// Inline token (`COOP_NATS_TOKEN`); fallback when no token file is set.
    pub nats_token: Option<String>,
    /// Token-file path (`COOP_NATS_TOKEN_FILE`). Re-read on every connect so a
    /// thaw uses the kubelet-refreshed token, not the value captured at boot.
    pub nats_token_file: Option<PathBuf>,
    pub nats_user: Option<String>,
    pub nats_password: Option<String>,
    pub nats_creds: Option<PathBuf>,
}

impl NatsParams {
    /// Resolve the auth token, re-reading the token file when configured (the
    /// in-cluster tmpfs Secret mount, W5). Mirrors `Config::nats_token_resolved`
    /// but operates on the captured params so reconnect re-reads from disk.
    fn token_resolved(&self) -> Option<String> {
        if let Some(path) = self.nats_token_file.as_deref() {
            match std::fs::read_to_string(path) {
                Ok(s) => {
                    let trimmed = s.trim_end_matches(['\r', '\n', '\t', ' ']).to_owned();
                    if !trimmed.is_empty() {
                        return Some(trimmed);
                    }
                    tracing::warn!(
                        path = %path.display(),
                        "nats-supervisor: token file empty; falling back to inline token"
                    );
                }
                Err(e) => {
                    tracing::warn!(
                        path = %path.display(),
                        err = %e,
                        "nats-supervisor: token-file read failed; falling back to inline token"
                    );
                }
            }
        }
        self.nats_token.clone()
    }

    /// Build a fresh `NatsAuth` with a re-resolved token. A new value is built
    /// per transport per connect so no transport shares a stale token.
    fn auth(&self) -> NatsAuth {
        NatsAuth {
            token: self.token_resolved(),
            user: self.nats_user.clone(),
            password: self.nats_password.clone(),
            creds_path: self.nats_creds.as_deref().map(Into::into),
        }
    }
}

/// Per-transport connect result, surfaced so the thaw handler can decide
/// fail-closed vs best-effort (rehydration-design §5: N1+N2 required, N3+N4
/// best-effort).
#[derive(Debug, Default, Clone)]
pub struct ReconnectReport {
    pub publisher: TransportStatus,
    pub relay: TransportStatus,
    pub forward: TransportStatus,
    pub inbox: TransportStatus,
}

/// Status of a single transport after a (re)connect attempt.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub enum TransportStatus {
    /// Not requested by the params (flag off / no inbox agent).
    #[default]
    Skipped,
    /// Connected and its run task spawned.
    Connected,
    /// `connect()` failed; the carried string is the error.
    Failed(String),
}

impl TransportStatus {
    /// Map to the per-stage status string used in the thaw ack (`thaw::Outcome`).
    pub fn as_stage(&self) -> &'static str {
        match self {
            TransportStatus::Skipped => "skipped",
            TransportStatus::Connected => "ok",
            TransportStatus::Failed(_) => "failed",
        }
    }
}

/// Owns the four NATS transports and the generation token they run under.
///
/// A "generation" is one set of live connections sharing a child
/// [`CancellationToken`]. `reconnect_all` cancels the current generation (so the
/// old clients are dropped and their dead sockets closed) and spawns a new one.
pub struct NatsSupervisor {
    params: Mutex<NatsParams>,
    store: Arc<Store>,
    /// Parent token: the process-wide shutdown. Every generation is a child of
    /// this, so a process shutdown still tears down whatever generation is live.
    parent: CancellationToken,
    /// The current generation's token. Cancelling it stops that generation's
    /// run tasks without touching the parent.
    generation: Mutex<Option<CancellationToken>>,
    /// Boot-time pre-resolved IP of the configured NATS host (bug
    /// ka-0000001492). The configured endpoint is a *bare* single-label service
    /// name (`nats://gardenio-nats:4222`); on a post-restore cold CNI netns a
    /// `getaddrinfo` of that bare name walks the full `ndots:5` search list with
    /// multi-second per-query timeouts before the netns has egress to kube-dns,
    /// so a single reconnect `connect()` can burn most of the thaw deadline on
    /// one slow lookup. We resolve the host ONCE at boot while DNS is warm and
    /// cache the IP in RAM; a gVisor freeze snapshots this value, so on thaw the
    /// reconnect dials the cached IP literal (the ClusterIP VIP is stable for the
    /// service's lifetime) — zero DNS on the cold path. `None` until prewarm
    /// succeeds, or when the endpoint is already an IP / not pre-resolvable, in
    /// which case the reconnect falls back to the bare-name retry (fix-C).
    ///
    /// Stored as `(host, ip)` so the thaw rewrite only fires when the effective
    /// reconnect URL still targets the host we pre-resolved.
    cached_ip: Mutex<Option<(String, IpAddr)>>,
}

impl NatsSupervisor {
    pub fn new(params: NatsParams, store: Arc<Store>, parent: CancellationToken) -> Arc<Self> {
        Arc::new(Self {
            params: Mutex::new(params),
            store,
            parent,
            generation: Mutex::new(None),
            cached_ip: Mutex::new(None),
        })
    }

    /// Resolve the configured NATS host to an IP and cache it in RAM for the
    /// post-restore reconnect (bug ka-0000001492). Called at boot, when DNS is
    /// warm; the resolved IP survives a gVisor freeze in the supervisor's heap so
    /// the thaw reconnect needs no cold-netns DNS. Best-effort and bounded: a
    /// failure (DNS not yet ready even at boot, or an already-IP endpoint) just
    /// leaves the cache empty and the thaw falls back to the bare-name retry.
    ///
    /// Idempotent and safe to re-run; a later success overwrites a stale cache.
    pub async fn prewarm_dns(&self) {
        let url = self.params.lock().await.url.clone();
        let Some(host) = host_of_url(&url) else {
            return; // already an IP literal, loopback ws, or unparseable host
        };
        // `lookup_host` needs a port; reuse 4222 (the resolve only cares about
        // the A/AAAA record, the port is irrelevant to the cached IP).
        let probe = format!("{host}:4222");
        let resolved =
            tokio::time::timeout(Duration::from_secs(5), tokio::net::lookup_host(probe)).await;
        match resolved {
            Ok(Ok(mut addrs)) => {
                if let Some(addr) = addrs.next() {
                    let ip = addr.ip();
                    tracing::info!(
                        host = %host,
                        ip = %ip,
                        "nats-supervisor: pre-resolved NATS host for thaw reconnect"
                    );
                    *self.cached_ip.lock().await = Some((host, ip));
                }
            }
            Ok(Err(e)) => {
                tracing::warn!(host = %host, err = %e, "nats-supervisor: boot DNS prewarm failed");
            }
            Err(_) => {
                tracing::warn!(host = %host, "nats-supervisor: boot DNS prewarm timed out");
            }
        }
    }

    /// If `url`'s host matches the boot-prewarmed host, return `url` with the
    /// host swapped for the cached IP literal; else `None` (keep the original).
    /// The host-equality guard ensures we never apply a stale ClusterIP to a
    /// different endpoint.
    async fn cached_ip_rewrite(&self, url: &str) -> Option<String> {
        let (host, ip) = self.cached_ip.lock().await.clone()?;
        if host_of_url(url).as_deref() != Some(host.as_str()) {
            return None;
        }
        rewrite_host_to_ip(url, ip)
    }

    /// Resolve the URL a single thaw reconnect *attempt* should dial (bug
    /// ka-0000001492). Most attempts take the boot-cached IP fast path
    /// ([`cached_ip_rewrite`]) for zero cold-netns DNS. But every
    /// [`RERESOLVE_EVERY`]th attempt (and attempt 0 only when there is no usable
    /// cache) re-resolves the bare host *live* and, on success, rewrites to the
    /// freshly-resolved IP — so a boot cache that is missing, stale, or pointing
    /// at a now-unrouteable address self-heals instead of pinning every retry to
    /// a dead literal. A live re-resolve that succeeds also refreshes the cache
    /// so subsequent fast-path attempts dial the good IP. Bounded by
    /// [`RERESOLVE_TIMEOUT`]; on miss/timeout we fall back to the cached IP, then
    /// to the bare name (which `connect()` resolves itself).
    async fn thaw_dial_url(&self, url: &str, attempt: u32) -> String {
        let cached = self.cached_ip_rewrite(url).await;
        let want_reresolve =
            attempt.is_multiple_of(RERESOLVE_EVERY) && (attempt > 0 || cached.is_none());
        if want_reresolve {
            if let Some(host) = host_of_url(url) {
                let probe = format!("{host}:4222");
                if let Ok(Ok(mut addrs)) =
                    tokio::time::timeout(RERESOLVE_TIMEOUT, tokio::net::lookup_host(probe)).await
                {
                    if let Some(addr) = addrs.next() {
                        let ip = addr.ip();
                        if let Some(rewritten) = rewrite_host_to_ip(url, ip) {
                            // Refresh the cache so the fast path dials the good IP.
                            *self.cached_ip.lock().await = Some((host, ip));
                            tracing::info!(
                                attempt,
                                to = %rewritten,
                                "nats-supervisor: thaw reconnect re-resolved NATS host live (cache self-heal)"
                            );
                            return rewritten;
                        }
                    }
                }
                tracing::warn!(
                    attempt,
                    "nats-supervisor: thaw live re-resolve missed; falling back to cached IP / bare name"
                );
            }
        }
        cached.unwrap_or_else(|| url.to_owned())
    }

    /// Spawn the first generation of transports (boot path). Best-effort: a
    /// failed transport logs a warning and leaves the others running, matching
    /// the original inline boot behaviour. Single-shot — the boot network is
    /// already warm, so no retry deadline is threaded (see `spawn_generation`).
    pub async fn connect_all(&self) -> ReconnectReport {
        self.spawn_generation(None).await
    }

    /// Test-only: the URL the thaw reconnect would actually dial for the current
    /// endpoint, applying the cached-IP rewrite exactly as `spawn_generation`
    /// does on the thaw path. Lets unit tests assert the rewrite decision
    /// without a live broker.
    #[cfg(test)]
    async fn effective_thaw_url(&self) -> Option<String> {
        let url = self.params.lock().await.url.clone();
        Some(self.cached_ip_rewrite(&url).await.unwrap_or(url))
    }

    /// Test-only: the currently-stored endpoint URL (asserts we never mutate it).
    #[cfg(test)]
    async fn current_url(&self) -> String {
        self.params.lock().await.url.clone()
    }

    /// Tear down the current generation and spawn a fresh one with a re-resolved
    /// token and, when `new_url` is `Some`, a new endpoint (the thaw handler
    /// passes `None`; the endpoint swap lets the E2E suite stand in for a pod's
    /// IP change). Idempotent: safe to call repeatedly (a thaw double-fire just
    /// rebuilds one extra generation).
    ///
    /// `retry_deadline` bounds the per-transport reconnect retry (the thaw path
    /// passes the marker-derived deadline so the required transports re-probe a
    /// cold-netns DNS miss under budget; `None` keeps the boot path single-shot).
    pub async fn reconnect_all(
        &self,
        new_url: Option<String>,
        retry_deadline: Option<tokio::time::Instant>,
    ) -> ReconnectReport {
        if let Some(url) = new_url {
            let mut p = self.params.lock().await;
            if url != p.url {
                tracing::info!(old = %p.url, new = %url, "nats-supervisor: endpoint updated for reconnect");
            }
            p.url = url;
        }

        // Cancel the previous generation FIRST so its dead async-nats clients are
        // dropped (closing the stale 4-tuple sockets) before we dial fresh ones.
        if let Some(prev) = self.generation.lock().await.take() {
            prev.cancel();
        }

        self.spawn_generation(retry_deadline).await
    }

    /// Build a fresh child token, spawn every requested transport under it, and
    /// store it as the current generation.
    ///
    /// `retry_deadline`, when set, bounds the *required* transports (publisher,
    /// relay) to retry their `connect()` under a capped backoff until they
    /// succeed or the deadline passes — absorbing a transient cold-netns DNS
    /// `EAI_AGAIN` on a just-restored pod (bug ka-0000001492). Best-effort
    /// transports (forward, inbox) stay single-shot: the durable JetStream inbox
    /// self-heals and a transient forward miss is not worth eating the budget.
    async fn spawn_generation(
        &self,
        retry_deadline: Option<tokio::time::Instant>,
    ) -> ReconnectReport {
        let params = self.params.lock().await.clone();
        let is_thaw = retry_deadline.is_some();

        // Thaw path only: cap each required-transport connect attempt short so a
        // SYN black-holed on a not-yet-routable post-restore netns fails fast and
        // the bounded retry re-probes often (bug ka-0000001492). `None` on boot
        // keeps async-nats's 5 s default.
        let connect_timeout = retry_deadline.map(|_| thaw_connect_timeout());

        // Best-effort transports (forward N3, inbox N4) are single-shot, so they
        // get the one-shot cached-IP rewrite on thaw (zero cold-netns DNS) with a
        // bare-name fallback. The *required* transports (publisher, relay) resolve
        // their dial URL per attempt inside `connect_with_retry` so a missing or
        // stale boot cache self-heals via a periodic live re-resolve — they keep
        // the un-rewritten bare endpoint here and decide per attempt. The boot
        // path (`is_thaw == false`) keeps the bare name everywhere: DNS is warm
        // and we must not change boot semantics. We never overwrite the stored
        // endpoint, only local clones.
        let mut be_params = params.clone();
        if is_thaw {
            if let Some(rewritten) = self.cached_ip_rewrite(&be_params.url).await {
                be_params.url = rewritten;
            }
        }

        let gen = self.parent.child_token();
        let mut report = ReconnectReport::default();

        if params.publisher {
            report.publisher =
                self.spawn_publisher(&params, &gen, retry_deadline, connect_timeout).await;
        }
        if params.relay {
            report.relay = self.spawn_relay(&params, &gen, retry_deadline, connect_timeout).await;
        }
        if params.forward {
            report.forward = self.spawn_forward(&be_params, &gen).await;
        }
        if params.inbox_agent.is_some() {
            report.inbox = self.spawn_inbox(&be_params, &gen).await;
        }

        *self.generation.lock().await = Some(gen);
        report
    }

    async fn spawn_publisher(
        &self,
        p: &NatsParams,
        gen: &CancellationToken,
        retry_deadline: Option<tokio::time::Instant>,
        connect_timeout: Option<Duration>,
    ) -> TransportStatus {
        let is_thaw = retry_deadline.is_some();
        match connect_with_retry(retry_deadline, "nats-publisher", |attempt| async move {
            // Boot: dial the configured endpoint as-is. Thaw: resolve per attempt
            // (cached IP fast path + periodic live re-resolve self-heal).
            let url =
                if is_thaw { self.thaw_dial_url(&p.url, attempt).await } else { p.url.clone() };
            // Boot enables background initial-connect retry so a lazily-warming
            // overlay circuit self-heals; thaw keeps fail-fast so its own bounded
            // retry loop re-resolves a fresh token per attempt (ka-0000008143).
            NatsPublisher::connect(
                &url,
                &p.prefix,
                &p.agent,
                &p.labels,
                p.auth(),
                connect_timeout,
                !is_thaw,
            )
            .await
        })
        .await
        {
            Ok(publisher) => {
                let store = Arc::clone(&self.store);
                let sd = gen.clone();
                tokio::spawn(async move {
                    publisher.run(&store, sd).await;
                });
                tracing::info!("nats-publisher: started");
                TransportStatus::Connected
            }
            Err(e) => {
                tracing::warn!("nats-publisher: failed to connect: {e}");
                TransportStatus::Failed(e.to_string())
            }
        }
    }

    async fn spawn_relay(
        &self,
        p: &NatsParams,
        gen: &CancellationToken,
        retry_deadline: Option<tokio::time::Instant>,
        connect_timeout: Option<Duration>,
    ) -> TransportStatus {
        let is_thaw = retry_deadline.is_some();
        match connect_with_retry(retry_deadline, "nats-relay", |attempt| async move {
            let url =
                if is_thaw { self.thaw_dial_url(&p.url, attempt).await } else { p.url.clone() };
            // Boot enables background initial-connect retry: the relay is the ONLY
            // presence path for outbound-only (MicroVM) runtimes and its
            // COOP_NATS_URL rides a Ziti circuit that warms up lazily, so a
            // one-shot boot connect that loses the race would leave the session
            // silently off the mux for its whole life. Thaw keeps fail-fast so its
            // bounded retry loop re-resolves a fresh token per attempt
            // (ka-0000008143).
            NatsRelay::connect(
                &url,
                &p.relay_prefix,
                &p.agent,
                &p.labels,
                p.auth(),
                connect_timeout,
                !is_thaw,
            )
            .await
        })
        .await
        {
            Ok(relay) => {
                // Grab a client clone + prefix before relay moves into its task.
                let sub_client = relay.client();
                let sub_prefix = relay.prefix().to_owned();

                let store = Arc::clone(&self.store);
                let sd = gen.clone();
                tokio::spawn(async move {
                    // run() publishes an eager `online` announce on entry, so a
                    // reconnect re-registers presence immediately (C4).
                    relay.run(store, sd).await;
                });

                let subscriber = NatsRelaySubscriber::new(sub_client, sub_prefix);
                let store = Arc::clone(&self.store);
                let sd = gen.clone();
                tokio::spawn(async move {
                    subscriber.run(store, sd).await;
                });

                tracing::info!("nats-relay: publisher + subscriber started");
                TransportStatus::Connected
            }
            Err(e) => {
                tracing::warn!("nats-relay: failed to connect: {e}");
                TransportStatus::Failed(e.to_string())
            }
        }
    }

    async fn spawn_forward(&self, p: &NatsParams, gen: &CancellationToken) -> TransportStatus {
        match NatsForwarder::connect(&p.url, &p.prefix, &p.agent, &p.labels, p.auth()).await {
            Ok(forwarder) => {
                let store = Arc::clone(&self.store);
                let sd = gen.clone();
                tokio::spawn(async move {
                    forwarder.run(&store, sd).await;
                });
                tracing::info!("nats-forward: message forwarder started");
                TransportStatus::Connected
            }
            Err(e) => {
                tracing::warn!("nats-forward: failed to connect: {e}");
                TransportStatus::Failed(e.to_string())
            }
        }
    }

    async fn spawn_inbox(&self, p: &NatsParams, gen: &CancellationToken) -> TransportStatus {
        let Some(ref agent_name) = p.inbox_agent else {
            return TransportStatus::Skipped;
        };
        match crate::transport::inbox::InboxConsumer::connect(
            &p.url,
            agent_name,
            p.inbox_rig.as_deref(),
            &p.inject_dir,
            p.auth(),
        )
        .await
        {
            Ok(consumer) => {
                let store = Arc::clone(&self.store);
                let sd = gen.clone();
                tokio::spawn(async move {
                    consumer.run(store, sd).await;
                });
                tracing::info!("inbox: consumer started");
                TransportStatus::Connected
            }
            Err(e) => {
                tracing::warn!("inbox: failed to connect consumer: {e}");
                TransportStatus::Failed(e.to_string())
            }
        }
    }
}

/// Thaw rehydration handler (Stage 3+4): on epoch increment, reconnect every
/// NATS transport with a freshly-resolved token and re-announce presence.
///
/// Fail-closed policy (rehydration-design §5): the **publisher (N1)** and
/// **relay (N2)** are *required* — the relay drives presence, so losing it makes
/// the agent invisible to the roster. A required transport failing to connect
/// fail-closes the thaw (`Outcome.ok == false`), so the controller's deadline
/// gate refuses to release the agent under ambiguous connectivity rather than
/// silently running disconnected. The **forwarder (N3)** and **inbox (N4)** are
/// best-effort — the durable JetStream inbox self-heals from its last ack — so a
/// failure there is recorded in the per-stage map but does not fail the thaw.
///
/// Idempotent: re-running for the same logical thaw just rebuilds one extra
/// generation of clients; the previous generation is cancelled first.
impl crate::thaw::RehydrateHandler for Arc<NatsSupervisor> {
    async fn rehydrate(&self, marker: &crate::thaw::Marker) -> crate::thaw::Outcome {
        // Anchor the retry budget to handler ENTRY so the required-transport
        // retry window stays strictly inside `run_handler`'s outer
        // `timeout(marker.deadline())`. The 0.85 fraction leaves headroom for the
        // spawn + ack write.
        let retry_deadline =
            Some(tokio::time::Instant::now() + marker.deadline().mul_f32(RECONNECT_RETRY_FRACTION));

        // Retry the required transports' connect() under the entry-anchored
        // deadline so a transient cold-netns DNS `EAI_AGAIN` on a just-restored
        // pod is absorbed instead of fail-closing the thaw (bug ka-0000001492).
        let report = self.reconnect_all(None, retry_deadline).await;

        let mut stages = std::collections::BTreeMap::new();
        stages.insert("nats_publisher".to_owned(), report.publisher.as_stage().to_owned());
        stages.insert("nats_relay".to_owned(), report.relay.as_stage().to_owned());
        stages.insert("nats_forward".to_owned(), report.forward.as_stage().to_owned());
        stages.insert("nats_inbox".to_owned(), report.inbox.as_stage().to_owned());

        // Required = publisher + relay. A required transport that was requested
        // but failed fail-closes the thaw.
        let mut failures = Vec::new();
        if let TransportStatus::Failed(ref e) = report.publisher {
            failures.push(format!("publisher: {e}"));
        }
        if let TransportStatus::Failed(ref e) = report.relay {
            failures.push(format!("relay: {e}"));
        }

        if failures.is_empty() {
            crate::thaw::Outcome { ok: true, error: None, stages }
        } else {
            crate::thaw::Outcome {
                ok: false,
                error: Some(format!("required NATS transport(s) failed: {}", failures.join("; "))),
                stages,
            }
        }
    }
}

#[cfg(test)]
#[path = "nats_supervisor_tests.rs"]
mod tests;
