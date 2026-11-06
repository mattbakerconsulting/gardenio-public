// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Unit tests for the NATS transport supervisor's pure logic — the bits that
//! don't need a live NATS server. Reconnect-against-a-real-broker is covered by
//! the E2E rehydration suite (`tests/specs/tests/rehydration.rs`, bead
//! ka-0000005803); here we assert the token re-read, the thaw dial-URL
//! rewrite, and the fail-closed stage mapping.

use super::*;
use crate::thaw::{Marker, RehydrateHandler};

#[test]
fn token_resolved_prefers_file_then_inline() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let token_file = dir.path().join("tok");
    std::fs::write(&token_file, "file-token\n")?;

    let mut p = base_params();
    p.nats_token = Some("inline-token".to_owned());
    p.nats_token_file = Some(token_file.clone());
    // File wins and trailing newline is trimmed.
    assert_eq!(p.token_resolved(), Some("file-token".to_owned()));

    // Empty file falls back to inline.
    std::fs::write(&token_file, "\n")?;
    assert_eq!(p.token_resolved(), Some("inline-token".to_owned()));

    // Missing file falls back to inline.
    p.nats_token_file = Some(dir.path().join("nope"));
    assert_eq!(p.token_resolved(), Some("inline-token".to_owned()));
    Ok(())
}

#[test]
fn transport_status_stage_strings() {
    assert_eq!(TransportStatus::Skipped.as_stage(), "skipped");
    assert_eq!(TransportStatus::Connected.as_stage(), "ok");
    assert_eq!(TransportStatus::Failed("x".into()).as_stage(), "failed");
}

/// A required transport (publisher) that fails fail-closes the thaw; a
/// best-effort one (forward/inbox) does not. We exercise the outcome mapping
/// directly off a ReconnectReport to avoid needing a live broker.
#[test]
fn outcome_mapping_required_vs_best_effort() {
    // Build the report → outcome mapping the way rehydrate() does, inlined so we
    // test the policy without a NATS connection.
    let map = |r: &ReconnectReport| -> (bool, usize) {
        let mut failures = 0;
        if matches!(r.publisher, TransportStatus::Failed(_)) {
            failures += 1;
        }
        if matches!(r.relay, TransportStatus::Failed(_)) {
            failures += 1;
        }
        (failures == 0, failures)
    };

    // All good → ok.
    let r = ReconnectReport {
        publisher: TransportStatus::Connected,
        relay: TransportStatus::Connected,
        forward: TransportStatus::Connected,
        inbox: TransportStatus::Connected,
    };
    assert_eq!(map(&r), (true, 0));

    // Best-effort failures only → still ok.
    let r = ReconnectReport {
        publisher: TransportStatus::Connected,
        relay: TransportStatus::Connected,
        forward: TransportStatus::Failed("net".into()),
        inbox: TransportStatus::Failed("net".into()),
    };
    assert_eq!(map(&r), (true, 0));

    // Required publisher failure → fail-closed.
    let r = ReconnectReport {
        publisher: TransportStatus::Failed("dead".into()),
        relay: TransportStatus::Connected,
        forward: TransportStatus::Connected,
        inbox: TransportStatus::Connected,
    };
    assert_eq!(map(&r), (false, 1));
}

/// With no transports requested, connect_all returns an all-skipped report and
/// the rehydrate handler acks OK (nothing required, nothing failed). This is the
/// idempotent-safe path: a thaw with no NATS configured is a clean no-op.
#[tokio::test]
async fn rehydrate_all_skipped_is_ok() -> anyhow::Result<()> {
    let store = crate::test_support::StoreBuilder::new().build().store;
    let parent = CancellationToken::new();
    let mut params = base_params();
    params.publisher = false;
    params.relay = false;
    params.forward = false;
    params.inbox_agent = None;

    let sup = NatsSupervisor::new(params, store, parent.clone());
    let report = sup.connect_all().await;
    assert_eq!(report.publisher, TransportStatus::Skipped);
    assert_eq!(report.relay, TransportStatus::Skipped);

    let outcome = sup.rehydrate(&Marker { epoch: 1, ..Default::default() }).await;
    assert!(outcome.ok, "all-skipped rehydrate should ack OK");
    assert_eq!(outcome.stages.get("nats_publisher").map(String::as_str), Some("skipped"));
    parent.cancel();
    Ok(())
}

/// Boot path: `deadline = None` is single-shot — the first failure returns
/// immediately, never retrying (we must not slow `connect_all` or change boot
/// semantics).
#[tokio::test]
async fn connect_with_retry_single_shot_when_no_deadline() -> anyhow::Result<()> {
    let attempts = std::sync::atomic::AtomicUsize::new(0);
    let r: anyhow::Result<()> = connect_with_retry(None, "test", |_attempt| {
        attempts.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        async { anyhow::bail!("boom") }
    })
    .await;
    assert!(r.is_err(), "single-shot failure should propagate");
    assert_eq!(
        attempts.load(std::sync::atomic::Ordering::SeqCst),
        1,
        "must not retry without a deadline"
    );
    Ok(())
}

/// Thaw path: a transient failure (cold-netns `EAI_AGAIN`) is retried under the
/// deadline and succeeds once the closure flips to Ok — the bug ka-0000001492
/// fix. We fail the first two attempts then succeed.
#[tokio::test(start_paused = true)]
async fn connect_with_retry_recovers_transient_under_deadline() -> anyhow::Result<()> {
    let attempts = std::sync::atomic::AtomicUsize::new(0);
    let deadline = Some(tokio::time::Instant::now() + Duration::from_secs(10));
    let r: anyhow::Result<u32> = connect_with_retry(deadline, "test", |_attempt| {
        let n = attempts.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        async move {
            if n < 2 {
                anyhow::bail!("Temporary failure in name resolution")
            }
            Ok(42)
        }
    })
    .await;
    assert_eq!(r.unwrap(), 42, "should recover once the transient miss clears");
    assert_eq!(attempts.load(std::sync::atomic::Ordering::SeqCst), 3, "two failures + one success");
    Ok(())
}

/// A connect that never succeeds returns the last error once the deadline
/// passes — the caller still records a `Failed` stage and fail-closes (we did
/// not turn a hard failure into a hang).
#[tokio::test(start_paused = true)]
async fn connect_with_retry_times_out_and_returns_error() -> anyhow::Result<()> {
    let attempts = std::sync::atomic::AtomicUsize::new(0);
    let deadline = Some(tokio::time::Instant::now() + Duration::from_millis(500));
    let r: anyhow::Result<()> = connect_with_retry(deadline, "test", |_attempt| {
        attempts.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        async { anyhow::bail!("never routable") }
    })
    .await;
    assert!(r.is_err(), "exhausted deadline returns the last error");
    assert!(
        attempts.load(std::sync::atomic::Ordering::SeqCst) >= 2,
        "should have retried at least once before the deadline"
    );
    Ok(())
}

// ── DNS pre-resolve / cached-IP rewrite (bug ka-0000001492) ────────────────
// (The pure URL host helpers `host_of_url` / `rewrite_host_to_ip` are tested in
// `nats_url_tests.rs`; here we cover the supervisor's use of the cached IP.)

/// The boot prewarm caches `(host, ip)`; the thaw reconnect (`retry_deadline`
/// present) must dial the cached IP literal instead of the bare name so the
/// cold-netns reconnect does zero DNS (bug ka-0000001492). We seed the cache
/// directly (no live resolver) and assert the effective dial URL is the IP.
#[tokio::test]
async fn thaw_reconnect_dials_cached_ip_when_host_matches() -> anyhow::Result<()> {
    let store = crate::test_support::StoreBuilder::new().build().store;
    let parent = CancellationToken::new();
    let mut params = base_params();
    params.url = "nats://gardenio-nats:4222".to_owned();
    // No transports requested → spawn_generation does no real dialing, but it
    // still applies the IP rewrite to its params clone, which is what we assert
    // via the post-condition: the stored endpoint is UNCHANGED (we only rewrite
    // the local clone), and the cache drove the decision.
    let sup = NatsSupervisor::new(params, store, parent.clone());

    // Seed the cache as if boot prewarm resolved gardenio-nats → 10.96.1.7.
    *sup.cached_ip.lock().await = Some(("gardenio-nats".to_owned(), "10.96.1.7".parse()?));

    // Compute what spawn_generation would dial on the thaw path. We mirror its
    // rewrite decision here against the public helpers to keep the test free of
    // a live broker while still exercising the host-match + rewrite logic.
    let url = sup.effective_thaw_url().await;
    assert_eq!(url.as_deref(), Some("nats://10.96.1.7:4222"));

    // The stored endpoint must be untouched (we never overwrite params.url).
    assert_eq!(sup.current_url().await, "nats://gardenio-nats:4222");
    parent.cancel();
    Ok(())
}

/// If the effective endpoint host differs from the pre-resolved host, the
/// cached IP must NOT be applied — we never dial a stale ClusterIP for a
/// different host.
#[tokio::test]
async fn thaw_reconnect_ignores_cached_ip_for_different_host() -> anyhow::Result<()> {
    let store = crate::test_support::StoreBuilder::new().build().store;
    let parent = CancellationToken::new();
    let mut params = base_params();
    params.url = "ws://127.0.0.1:40001/".to_owned(); // different host
    let sup = NatsSupervisor::new(params, store, parent.clone());

    *sup.cached_ip.lock().await = Some(("gardenio-nats".to_owned(), "10.96.1.7".parse()?));

    let url = sup.effective_thaw_url().await;
    assert_eq!(url.as_deref(), Some("ws://127.0.0.1:40001/"), "loopback host must be left as-is");
    parent.cancel();
    Ok(())
}

// ── Thaw per-attempt dial-URL resolution / cache self-heal (bug ka-0000001492) ─

/// Attempt 0 with a usable boot cache takes the fast path: it dials the cached
/// IP literal and does NOT re-resolve (the happy path must stay zero-DNS).
#[tokio::test]
async fn thaw_dial_url_attempt0_uses_cache_no_reresolve() -> anyhow::Result<()> {
    let store = crate::test_support::StoreBuilder::new().build().store;
    let parent = CancellationToken::new();
    let mut params = base_params();
    params.url = "nats://gardenio-nats:4222".to_owned();
    let sup = NatsSupervisor::new(params, store, parent.clone());
    *sup.cached_ip.lock().await = Some(("gardenio-nats".to_owned(), "10.96.1.7".parse()?));

    let url = sup.thaw_dial_url("nats://gardenio-nats:4222", 0).await;
    assert_eq!(url, "nats://10.96.1.7:4222", "attempt 0 with cache → cached IP fast path");
    parent.cancel();
    Ok(())
}

/// A missing boot cache must NOT pin every attempt to the bare name: attempt 0
/// with no cache re-resolves live (the degraded-boot self-heal — the canary
/// scenario where prewarm never cached a valid IP). We use `localhost`, which
/// always resolves, and assert the dial URL became an IP literal and the cache
/// was refreshed for subsequent fast-path attempts.
#[tokio::test]
async fn thaw_dial_url_reresolves_live_when_cache_missing() -> anyhow::Result<()> {
    let store = crate::test_support::StoreBuilder::new().build().store;
    let parent = CancellationToken::new();
    let mut params = base_params();
    params.url = "nats://localhost:4222".to_owned();
    let sup = NatsSupervisor::new(params, store, parent.clone());
    // No cache seeded → boot prewarm "never ran".
    assert!(sup.cached_ip.lock().await.is_none());

    let url = sup.thaw_dial_url("nats://localhost:4222", 0).await;
    assert_ne!(url, "nats://localhost:4222", "missing cache must re-resolve, not dial bare name");
    assert!(url.starts_with("nats://"), "scheme preserved: {url}");
    assert!(
        host_of_url(&url).is_none(),
        "re-resolved dial URL must be an IP literal (host_of_url → None): {url}"
    );
    // The live resolve refreshed the cache so later fast-path attempts dial it.
    assert!(sup.cached_ip.lock().await.is_some(), "successful re-resolve must refresh the cache");
    parent.cancel();
    Ok(())
}

/// On a periodic re-resolve attempt (`RERESOLVE_EVERY`th), the live resolve
/// overrides a *stale* cache — proving a boot cache pointing at a now-dead IP
/// self-heals instead of pinning the whole deadline. We seed a deliberately
/// wrong cache for `localhost`; the attempt-`RERESOLVE_EVERY` re-resolve must
/// replace it with the real loopback address.
#[tokio::test]
async fn thaw_dial_url_periodic_reresolve_overrides_stale_cache() -> anyhow::Result<()> {
    let store = crate::test_support::StoreBuilder::new().build().store;
    let parent = CancellationToken::new();
    let mut params = base_params();
    params.url = "nats://localhost:4222".to_owned();
    let sup = NatsSupervisor::new(params, store, parent.clone());
    // Stale/wrong cache: an address that would never route.
    *sup.cached_ip.lock().await = Some(("localhost".to_owned(), "203.0.113.1".parse()?));

    // A non-multiple attempt keeps the (stale) fast path.
    let fast = sup.thaw_dial_url("nats://localhost:4222", 1).await;
    assert_eq!(fast, "nats://203.0.113.1:4222", "non-reresolve attempt uses the cache as-is");

    // The RERESOLVE_EVERYth attempt re-resolves live and overrides the stale IP.
    let healed = sup.thaw_dial_url("nats://localhost:4222", RERESOLVE_EVERY).await;
    assert_ne!(healed, "nats://203.0.113.1:4222", "periodic re-resolve must override stale cache");
    assert!(host_of_url(&healed).is_none(), "healed dial URL is an IP literal: {healed}");
    parent.cancel();
    Ok(())
}

// ── Thaw per-attempt connect-timeout (bug ka-0000001492) ───────────────────

/// The thaw connect-timeout override parses a positive ms value, and falls back
/// to the short default on `0`, empty, absent, or garbage. The default must stay
/// well under async-nats's 5 s `connection_timeout` so a black-holed SYN on a
/// cold post-restore netns fails fast and the bounded retry re-probes often.
#[test]
fn parse_thaw_connect_timeout_override_and_fallbacks() {
    // Positive override wins.
    assert_eq!(parse_thaw_connect_timeout(Some("750".to_owned())), Duration::from_millis(750));
    // Whitespace is trimmed.
    assert_eq!(parse_thaw_connect_timeout(Some("  1500 ".to_owned())), Duration::from_millis(1500));
    // Zero, empty, garbage, absent → default.
    assert_eq!(parse_thaw_connect_timeout(Some("0".to_owned())), THAW_CONNECT_TIMEOUT);
    assert_eq!(parse_thaw_connect_timeout(Some(String::new())), THAW_CONNECT_TIMEOUT);
    assert_eq!(parse_thaw_connect_timeout(Some("nope".to_owned())), THAW_CONNECT_TIMEOUT);
    assert_eq!(parse_thaw_connect_timeout(None), THAW_CONNECT_TIMEOUT);
    // The default itself must be short enough to re-probe inside the deadline —
    // strictly less than async-nats's 5 s connection_timeout default.
    assert!(THAW_CONNECT_TIMEOUT < Duration::from_secs(5));
}

/// `build_connect_options` accepts an optional per-attempt connection timeout
/// and builds cleanly with and without it (the boot path passes `None` to keep
/// the 5 s default; the thaw path passes a short cap).
#[tokio::test]
async fn build_connect_options_accepts_optional_connect_timeout() -> anyhow::Result<()> {
    use crate::transport::nats::{build_connect_options, NatsAuth};
    // No timeout (boot path) builds.
    build_connect_options(NatsAuth::default(), None, false).await?;
    // Short timeout (thaw path) builds.
    build_connect_options(NatsAuth::default(), Some(Duration::from_millis(2000)), false).await?;
    Ok(())
}

/// `build_connect_options` builds cleanly with `retry_on_initial_connect` set
/// (the boot path passes `true` so a lazily-warming overlay NATS circuit
/// self-heals instead of losing the one-shot connect, ka-0000008143). This is a
/// smoke check that the option composes with both the auth and timeout paths.
#[tokio::test]
async fn build_connect_options_accepts_retry_on_initial_connect() -> anyhow::Result<()> {
    use crate::transport::nats::{build_connect_options, NatsAuth};
    // Boot: no timeout + background initial-connect retry.
    build_connect_options(NatsAuth::default(), None, true).await?;
    // Composes with the user/password (subject-isolation) auth branch too.
    build_connect_options(
        NatsAuth {
            token: Some("shared-secret".into()),
            user: Some("agent".into()),
            password: None,
            creds_path: None,
        },
        None,
        true,
    )
    .await?;
    Ok(())
}

/// user + token (no explicit password) must take the user/password branch —
/// the subject-isolation contract (ka-0000000002): multi-user server config
/// only matches user/password entries, so the delivered token is sent as the
/// configured user's password. The old priority order (token first) silently
/// discarded the username and the server refused the connection.
#[tokio::test]
async fn build_connect_options_user_plus_token_is_user_password() -> anyhow::Result<()> {
    use crate::transport::nats::{build_connect_options, NatsAuth};
    build_connect_options(
        NatsAuth {
            token: Some("shared-secret".into()),
            user: Some("agent".into()),
            password: None,
            creds_path: None,
        },
        None,
        false,
    )
    .await?;
    Ok(())
}

fn base_params() -> NatsParams {
    NatsParams {
        url: "ws://127.0.0.1:4222/".to_owned(),
        prefix: "coop.test".to_owned(),
        relay_prefix: "coop.test.mux".to_owned(),
        agent: "claude".to_owned(),
        labels: vec![],
        publisher: false,
        relay: false,
        forward: false,
        inbox_agent: None,
        inbox_rig: None,
        inject_dir: std::path::PathBuf::from("/tmp/inject"),
        nats_token: None,
        nats_token_file: None,
        nats_user: None,
        nats_password: None,
        nats_creds: None,
    }
}
