// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::HashMap;

use super::*;

fn entry(name: &str) -> ProfileEntry {
    ProfileEntry {
        name: name.to_owned(),
        credentials: HashMap::from([("API_KEY".to_owned(), format!("key-{name}"))]),
        usable: None,
    }
}

/// Like [`entry`] but with an explicit bead-health flag (ka-0000006430).
fn entry_usable(name: &str, usable: bool) -> ProfileEntry {
    ProfileEntry { usable: Some(usable), ..entry(name) }
}

/// Extract the SwitchRequest from a RotateOutcome::Switch, panicking otherwise.
fn unwrap_switch(outcome: RotateOutcome) -> SwitchRequest {
    match outcome {
        RotateOutcome::Switch(req) => req,
        other => panic!("expected Switch, got {other:?}"),
    }
}

#[tokio::test]
async fn register_seeds_active_on_first_then_upserts() -> anyhow::Result<()> {
    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b"), entry("c")]).await;

    // First registration into an empty set: first entry becomes active.
    let list = state.list().await;
    assert_eq!(list.len(), 3);
    assert_eq!(list[0].status, "active");
    assert_eq!(list[1].status, "available");
    assert_eq!(list[2].status, "available");
    assert_eq!(state.active_name().await.as_deref(), Some("a"));

    // A subsequent push (e.g. distributor account-add) is additive: the new
    // profile is appended as available and the active profile is preserved.
    // This is the ka-0000005975 fix — a single-profile push must NOT clobber
    // the session's reported active account.
    state.register(vec![entry("x")]).await;
    let list = state.list().await;
    assert_eq!(list.len(), 4);
    assert_eq!(state.active_name().await.as_deref(), Some("a"), "active must be preserved");
    assert_eq!(list[3].name, "x");
    assert_eq!(list[3].status, "available");
    Ok(())
}

#[tokio::test]
async fn register_refreshes_existing_credentials_without_touching_status() -> anyhow::Result<()> {
    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b")]).await;
    state.set_active("b").await;
    assert_eq!(state.active_name().await.as_deref(), Some("b"));

    // Re-pushing "b" with fresh credentials must refresh the creds but keep
    // "b" active and "a" available — no status churn.
    let mut refreshed = entry("b");
    refreshed.credentials.insert("API_KEY".to_owned(), "rotated-b".to_owned());
    state.register(vec![refreshed]).await;

    assert_eq!(state.active_name().await.as_deref(), Some("b"));
    let creds = state.resolve_credentials("b").await.expect("b should resolve");
    assert_eq!(creds.get("API_KEY").map(String::as_str), Some("rotated-b"));
    let list = state.list().await;
    assert_eq!(list.len(), 2);
    Ok(())
}

#[tokio::test]
async fn register_single_profile_does_not_clobber_active_label() -> anyhow::Result<()> {
    // Regression for ka-0000005975: an unrelated session has account "opus"
    // active; the distributor adds a brand-new account "first-mate-haiku" by
    // pushing a single-profile body. The active label must stay "opus".
    let state = ProfileState::new();
    state.register(vec![entry("opus")]).await;
    assert_eq!(state.active_name().await.as_deref(), Some("opus"));

    state.register(vec![entry("first-mate-haiku")]).await;
    assert_eq!(
        state.active_name().await.as_deref(),
        Some("opus"),
        "adding an account must not relabel the session"
    );
    Ok(())
}

#[tokio::test]
async fn try_auto_rotate_picks_next() -> anyhow::Result<()> {
    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b"), entry("c")]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"));
    assert!(req.force);
    assert!(req.credentials.is_some());

    // "a" should now be rate_limited, no one is active yet (set_active not called).
    let list = state.list().await;
    assert_eq!(list[0].status, "rate_limited");
    Ok(())
}

#[tokio::test]
async fn try_auto_rotate_skips_rate_limited() -> anyhow::Result<()> {
    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b"), entry("c")]).await;

    // Rotate once: a → rate_limited, picks b.
    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"));

    // Simulate: set b as active.
    state.set_active("b").await;

    // Rotate again: b → rate_limited, should skip a (still rate_limited), pick c.
    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("c"));
    Ok(())
}

#[tokio::test]
async fn try_auto_rotate_exhausted_when_all_limited() -> anyhow::Result<()> {
    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b")]).await;

    // Rotate: a → rate_limited, picks b.
    let req = unwrap_switch(state.try_auto_rotate().await);
    assert!(req.profile.is_some());

    // Set b as active, then rotate: b → rate_limited, a still rate_limited → Exhausted.
    state.set_active("b").await;
    let outcome = state.try_auto_rotate().await;
    match outcome {
        RotateOutcome::Exhausted { retry_after } => {
            // retry_after should be positive (cooldown_secs defaults to 300).
            assert!(retry_after.as_secs() > 0, "retry_after should be positive: {retry_after:?}");
        }
        other => panic!("expected Exhausted, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn try_auto_rotate_respects_anti_flap() -> anyhow::Result<()> {
    // `ProfileState::new()` uses `RotationConfig::default()` — anti-flap limit
    // 20/hr, cooldown 300s — independent of the ambient environment
    // (ka-0000003994). With the default limit of 20, a handful of rotations
    // stays well under the cap and each one should succeed.
    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b"), entry("c")]).await;

    // Two rotations should succeed.
    let r1 = unwrap_switch(state.try_auto_rotate().await);
    state.set_active(r1.profile.as_deref().unwrap()).await;

    let r2 = unwrap_switch(state.try_auto_rotate().await);
    state.set_active(r2.profile.as_deref().unwrap()).await;

    // With default max_switches_per_hour=20, this should still succeed.
    let r3 = state.try_auto_rotate().await;
    assert!(matches!(r3, RotateOutcome::Switch(_) | RotateOutcome::Exhausted { .. }));
    Ok(())
}

#[tokio::test]
async fn try_auto_rotate_disabled_by_mode() -> anyhow::Result<()> {
    let state = ProfileState::new();
    state.set_mode(ProfileMode::Manual);
    state.register(vec![entry("a"), entry("b")]).await;

    assert!(matches!(state.try_auto_rotate().await, RotateOutcome::Skipped));
    Ok(())
}

#[tokio::test]
async fn try_auto_rotate_needs_at_least_two_profiles() -> anyhow::Result<()> {
    let state = ProfileState::new();
    state.register(vec![entry("a")]).await;
    assert!(matches!(state.try_auto_rotate().await, RotateOutcome::Skipped));

    // No profiles at all.
    let empty = ProfileState::new();
    assert!(matches!(empty.try_auto_rotate().await, RotateOutcome::Skipped));
    Ok(())
}

#[tokio::test]
async fn set_active_tracks_profile() -> anyhow::Result<()> {
    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b")]).await;

    assert_eq!(state.active_name().await.as_deref(), Some("a"));

    state.set_active("b").await;
    assert_eq!(state.active_name().await.as_deref(), Some("b"));

    // "a" should no longer be active.
    let list = state.list().await;
    assert_eq!(list[0].status, "available");
    assert_eq!(list[1].status, "active");

    // Credentials resolve correctly for both profiles.
    let creds = state.resolve_credentials("b").await;
    assert!(creds.is_some());
    assert_eq!(creds.unwrap().get("API_KEY").unwrap(), "key-b");
    assert!(state.resolve_credentials("nonexistent").await.is_none());
    Ok(())
}

#[tokio::test]
async fn retry_pending_dedup() -> anyhow::Result<()> {
    let state = ProfileState::new();
    // Initially false.
    assert!(!state.retry_pending.load(std::sync::atomic::Ordering::Acquire));

    // First swap sets it to true, returns false (was not pending).
    let was_pending = state.retry_pending.swap(true, std::sync::atomic::Ordering::AcqRel);
    assert!(!was_pending);

    // Second swap returns true (already pending) — schedule_retry would bail.
    let was_pending = state.retry_pending.swap(true, std::sync::atomic::Ordering::AcqRel);
    assert!(was_pending);

    // Clear it.
    state.retry_pending.store(false, std::sync::atomic::Ordering::Release);
    let was_pending = state.retry_pending.swap(true, std::sync::atomic::Ordering::AcqRel);
    assert!(!was_pending);
    Ok(())
}

#[tokio::test]
async fn exhausted_retry_after_uses_shortest_cooldown() -> anyhow::Result<()> {
    // Default cooldown is 300s (RotationConfig::default), env-independent
    // (ka-0000003994).
    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b"), entry("c")]).await;

    // Exhaust a → rate_limited, picks b.
    let _r1 = unwrap_switch(state.try_auto_rotate().await);
    state.set_active("b").await;

    // Exhaust b → rate_limited, picks c.
    let _r2 = unwrap_switch(state.try_auto_rotate().await);
    state.set_active("c").await;

    // Exhaust c → all rate_limited → Exhausted.
    let outcome = state.try_auto_rotate().await;
    match outcome {
        RotateOutcome::Exhausted { retry_after } => {
            // retry_after should be positive.
            assert!(retry_after.as_secs() > 0, "retry_after should be positive");
        }
        other => panic!("expected Exhausted, got {other:?}"),
    }
    Ok(())
}

/// Regression for ka-0000003994: rotation must NOT read process-global env.
/// `ProfileState::new()` uses `RotationConfig::default()`, so a rotation
/// succeeds even when `COOP_ROTATE_MAX_PER_HOUR=0` is set in the ambient
/// environment (the gardenio agent runtime does exactly this). Before the fix
/// this test — and the five try_auto_rotate tests above — failed with
/// "expected Switch, got Skipped" whenever that var was set.
#[tokio::test]
#[serial_test::serial]
async fn try_auto_rotate_is_hermetic_against_env_var() -> anyhow::Result<()> {
    let prev = std::env::var("COOP_ROTATE_MAX_PER_HOUR").ok();
    std::env::set_var("COOP_ROTATE_MAX_PER_HOUR", "0");

    let state = ProfileState::new();
    state.register(vec![entry("a"), entry("b")]).await;

    // Default config (limit 20) must win over the ambient env var.
    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"));

    match prev {
        Some(v) => std::env::set_var("COOP_ROTATE_MAX_PER_HOUR", v),
        None => std::env::remove_var("COOP_ROTATE_MAX_PER_HOUR"),
    }
    Ok(())
}

/// The anti-flap limit is honoured when threaded through `RotationConfig`:
/// a limit of 0 makes the very first rotation Skip, mirroring the production
/// knob without touching process env.
#[tokio::test]
async fn rotation_config_zero_limit_skips() -> anyhow::Result<()> {
    let state = ProfileState::with_rotation(RotationConfig {
        max_switches_per_hour: 0,
        ..RotationConfig::default()
    });
    state.register(vec![entry("a"), entry("b")]).await;

    assert!(matches!(state.try_auto_rotate().await, RotateOutcome::Skipped));
    Ok(())
}

/// `RotationConfig::from_env` reads the knobs once; with the var set it reflects
/// that value, and with it unset it falls back to the default.
#[tokio::test]
#[serial_test::serial]
async fn rotation_config_from_env_reads_knobs() -> anyhow::Result<()> {
    let prev = std::env::var("COOP_ROTATE_MAX_PER_HOUR").ok();

    std::env::set_var("COOP_ROTATE_MAX_PER_HOUR", "7");
    assert_eq!(RotationConfig::from_env().max_switches_per_hour, 7);

    std::env::remove_var("COOP_ROTATE_MAX_PER_HOUR");
    assert_eq!(
        RotationConfig::from_env().max_switches_per_hour,
        RotationConfig::default().max_switches_per_hour
    );

    if let Some(v) = prev {
        std::env::set_var("COOP_ROTATE_MAX_PER_HOUR", v);
    }
    Ok(())
}

#[tokio::test]
async fn mode_get_set() -> anyhow::Result<()> {
    let state = ProfileState::new();
    assert_eq!(state.mode(), ProfileMode::Auto);

    state.set_mode(ProfileMode::Manual);
    assert_eq!(state.mode(), ProfileMode::Manual);

    state.set_mode(ProfileMode::Auto);
    assert_eq!(state.mode(), ProfileMode::Auto);
    Ok(())
}

// ---------------------------------------------------------------------------
// Pre-switch probe (ka-0000007302)
//
// Auto-rotate must never switch a session onto a candidate account whose
// credential is already dead. These tests inject a fake prober so the gate is
// exercised hermetically (no network).
// ---------------------------------------------------------------------------

/// Fake [`CredentialProber`] mapping profile name -> canned `ProbeStatus`.
/// Unknown names default to `Valid`. Records how many probes were issued so a
/// test can assert the gate was (or was not) consulted.
struct FakeProber {
    statuses: HashMap<String, crate::probe::ProbeStatus>,
    calls: std::sync::atomic::AtomicUsize,
}

impl FakeProber {
    fn new(pairs: &[(&str, crate::probe::ProbeStatus)]) -> Self {
        Self {
            statuses: pairs.iter().map(|(n, s)| ((*n).to_owned(), s.clone())).collect(),
            calls: std::sync::atomic::AtomicUsize::new(0),
        }
    }

    fn calls(&self) -> usize {
        self.calls.load(std::sync::atomic::Ordering::Acquire)
    }
}

impl CredentialProber for FakeProber {
    fn probe(
        &self,
        name: String,
        _credentials: HashMap<String, String>,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = crate::probe::ProbeStatus> + Send>>
    {
        self.calls.fetch_add(1, std::sync::atomic::Ordering::AcqRel);
        let status = self.statuses.get(&name).cloned().unwrap_or(crate::probe::ProbeStatus::Valid);
        Box::pin(async move { status })
    }
}

/// Build a probe-ENABLED profile state wired to the given fake prober.
fn probe_state(prober: std::sync::Arc<FakeProber>) -> ProfileState {
    ProfileState::with_rotation_and_prober(
        RotationConfig { preswitch_probe: true, ..RotationConfig::default() },
        prober,
    )
}

#[tokio::test]
async fn preswitch_probe_skips_invalid_candidate_and_picks_next() -> anyhow::Result<()> {
    // a active; first candidate b is dead (org-disabled / revoked -> Invalid),
    // c is healthy. Rotation must skip b and land on c.
    let prober = std::sync::Arc::new(FakeProber::new(&[
        ("b", crate::probe::ProbeStatus::Invalid),
        ("c", crate::probe::ProbeStatus::Valid),
    ]));
    let state = probe_state(prober.clone());
    state.register(vec![entry("a"), entry("b"), entry("c")]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("c"), "must skip Invalid b, pick Valid c");
    // Probed b (rejected) then c (accepted) -- exactly two probes.
    assert_eq!(prober.calls(), 2);

    // Rejected candidate b is cooled down so it is not retried immediately.
    let list = state.list().await;
    let b = list.iter().find(|p| p.name == "b").expect("b present");
    assert_eq!(b.status, "rate_limited", "probe-rejected b must be cooled down");
    Ok(())
}

#[tokio::test]
async fn preswitch_probe_all_invalid_is_exhausted() -> anyhow::Result<()> {
    // Only alternative b is dead -> no healthy target -> Exhausted (park + retry),
    // NOT a switch onto the dead account.
    let prober = std::sync::Arc::new(FakeProber::new(&[("b", crate::probe::ProbeStatus::Invalid)]));
    let state = probe_state(prober.clone());
    state.register(vec![entry("a"), entry("b")]).await;

    match state.try_auto_rotate().await {
        RotateOutcome::Exhausted { retry_after } => {
            assert!(retry_after.as_secs() > 0, "retry_after should be positive");
        }
        other => panic!("expected Exhausted (all candidates probe Invalid), got {other:?}"),
    }
    assert_eq!(prober.calls(), 1, "probed the single dead candidate");
    Ok(())
}

#[tokio::test]
async fn preswitch_probe_error_is_fail_open() -> anyhow::Result<()> {
    // A transient/inconclusive probe (timeout, 5xx) must NOT block the switch:
    // a flaky probe should never strand a session a real switch could recover.
    let prober = std::sync::Arc::new(FakeProber::new(&[("b", crate::probe::ProbeStatus::Error)]));
    let state = probe_state(prober);
    state.register(vec![entry("a"), entry("b")]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"), "Error probe must fail open to a switch");
    Ok(())
}

#[tokio::test]
async fn preswitch_probe_skipped_is_fail_open() -> anyhow::Result<()> {
    // A candidate with no probeable credential (Skipped) must also fail open.
    let prober = std::sync::Arc::new(FakeProber::new(&[("b", crate::probe::ProbeStatus::Skipped)]));
    let state = probe_state(prober);
    state.register(vec![entry("a"), entry("b")]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"), "Skipped probe must fail open to a switch");
    Ok(())
}

#[tokio::test]
async fn preswitch_probe_disabled_does_not_probe() -> anyhow::Result<()> {
    // With the gate OFF, rotation keeps its historical behaviour: it picks the
    // first available candidate WITHOUT probing -- even if that candidate would
    // have probed Invalid.
    let prober = std::sync::Arc::new(FakeProber::new(&[("b", crate::probe::ProbeStatus::Invalid)]));
    let state = ProfileState::with_rotation_and_prober(
        RotationConfig { preswitch_probe: false, ..RotationConfig::default() },
        prober.clone(),
    );
    state.register(vec![entry("a"), entry("b")]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"), "disabled gate keeps first-available pick");
    assert_eq!(prober.calls(), 0, "probe must not run when the gate is disabled");
    Ok(())
}

/// `RotationConfig::from_env` turns the pre-switch probe ON by default and
/// honours `COOP_ROTATE_PRESWITCH_PROBE=0` for rollback.
#[tokio::test]
#[serial_test::serial]
async fn rotation_config_from_env_preswitch_probe_default_on() -> anyhow::Result<()> {
    let prev = std::env::var("COOP_ROTATE_PRESWITCH_PROBE").ok();

    std::env::remove_var("COOP_ROTATE_PRESWITCH_PROBE");
    assert!(RotationConfig::from_env().preswitch_probe, "default ON");

    std::env::set_var("COOP_ROTATE_PRESWITCH_PROBE", "0");
    assert!(!RotationConfig::from_env().preswitch_probe, "=0 disables");

    std::env::set_var("COOP_ROTATE_PRESWITCH_PROBE", "1");
    assert!(RotationConfig::from_env().preswitch_probe, "=1 enables");

    match prev {
        Some(v) => std::env::set_var("COOP_ROTATE_PRESWITCH_PROBE", v),
        None => std::env::remove_var("COOP_ROTATE_PRESWITCH_PROBE"),
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Bead-authoritative health gate (ka-0000006430)
//
// coopmux stamps a `usable` flag on staged profiles from the account bead's
// status/throttle_status. `try_auto_rotate` must skip a profile the flag has
// condemned (an account that went exhausted AFTER it was staged — the fo85
// incident), while failing open when no flag is present. These tests exercise
// the gate purely on in-memory profile state (no network, no coopmux).
// ---------------------------------------------------------------------------

/// Build a profile state with the health gate ON but the pre-switch probe OFF,
/// so a test isolates the health filter from the network probe.
fn health_gate_state() -> ProfileState {
    ProfileState::with_rotation(RotationConfig {
        health_gate: true,
        preswitch_probe: false,
        ..RotationConfig::default()
    })
}

#[tokio::test]
async fn health_gate_skips_unusable_candidate_and_picks_next() -> anyhow::Result<()> {
    // The incident shape: `a` active, the round-robin-next staged profile `b`
    // was condemned by coopmux (usable=false) after going exhausted, `c` is
    // still usable. Rotation must skip `b` and land on `c`.
    let state = health_gate_state();
    state.register(vec![entry("a"), entry_usable("b", false), entry_usable("c", true)]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("c"), "must skip unusable b, pick usable c");

    // `b` is only filtered as a rotation target — it stays STAGED (Available),
    // not cooled down, so a switchback is trivial once coopmux re-marks it
    // usable. (Contrast the probe path, which cools down its rejects.)
    let list = state.list().await;
    let b = list.iter().find(|p| p.name == "b").expect("b present");
    assert_eq!(b.status, "available", "health-gated b stays staged, not cooled down");
    Ok(())
}

#[tokio::test]
async fn health_gate_all_unusable_is_exhausted() -> anyhow::Result<()> {
    // The only alternative is condemned → no usable target → Exhausted (park +
    // retry), NEVER a switch onto the dead account.
    let state = health_gate_state();
    state.register(vec![entry("a"), entry_usable("b", false)]).await;

    match state.try_auto_rotate().await {
        RotateOutcome::Exhausted { retry_after } => {
            assert!(retry_after.as_secs() > 0, "retry_after should be positive");
        }
        other => panic!("expected Exhausted (only candidate is unusable), got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn health_gate_fail_open_when_flag_absent() -> anyhow::Result<()> {
    // No health signal (usable defaults true) → historical behavior: pick the
    // first available candidate. A missing flag must never strand a session.
    let state = health_gate_state();
    state.register(vec![entry("a"), entry("b")]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"), "absent flag must fail open to a switch");
    Ok(())
}

#[tokio::test]
async fn health_gate_disabled_ignores_flag() -> anyhow::Result<()> {
    // With the gate OFF (kill switch), a condemned candidate is NOT filtered —
    // exact pre-fix behavior. This is the `COOP_ROTATE_HEALTH_GATE=0` rollback.
    let state = ProfileState::with_rotation(RotationConfig {
        health_gate: false,
        preswitch_probe: false,
        ..RotationConfig::default()
    });
    state.register(vec![entry("a"), entry_usable("b", false)]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"), "disabled gate keeps condemned b as a candidate");
    Ok(())
}

#[tokio::test]
async fn register_updates_usable_flag_on_upsert() -> anyhow::Result<()> {
    // A creds-refresh push that OMITS the flag (None) must NOT resurrect a
    // condemned profile; a push that CARRIES a flag updates it. This is the
    // refresh-loop re-push path: coopmux flips b unusable, then usable again.
    let state = health_gate_state();
    state.register(vec![entry("a"), entry("b"), entry("c")]).await;

    // coopmux condemns b (usable=false).
    state.register(vec![entry_usable("b", false)]).await;
    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("c"), "condemned b skipped");

    // A creds-only refresh of b (usable=None) must leave it condemned.
    state.register(vec![entry("b")]).await;
    // a is now rate_limited from the rotation above; reset by registering fresh
    // state to isolate the flag semantics.
    let state2 = health_gate_state();
    state2.register(vec![entry("a"), entry("b"), entry("c")]).await;
    state2.register(vec![entry_usable("b", false)]).await;
    state2.register(vec![entry("b")]).await; // creds-only refresh, flag untouched
    let req = unwrap_switch(state2.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("c"), "creds-only refresh must not un-condemn b");

    // coopmux re-marks b usable on recovery → b eligible again.
    let state3 = health_gate_state();
    state3.register(vec![entry("a"), entry("b"), entry("c")]).await;
    state3.register(vec![entry_usable("b", false)]).await;
    state3.register(vec![entry_usable("b", true)]).await; // recovery re-push
    let req = unwrap_switch(state3.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("b"), "recovered b is a candidate again");
    Ok(())
}

#[tokio::test]
async fn health_gate_and_probe_compose() -> anyhow::Result<()> {
    // Both gates on: b is bead-condemned (usable=false) and never even reaches
    // the probe; c probes Valid and wins. Confirms the health filter runs in
    // phase-1 (candidate collection), upstream of the phase-2 probe.
    let prober = std::sync::Arc::new(FakeProber::new(&[("c", crate::probe::ProbeStatus::Valid)]));
    let state = ProfileState::with_rotation_and_prober(
        RotationConfig { health_gate: true, preswitch_probe: true, ..RotationConfig::default() },
        prober.clone(),
    );
    state.register(vec![entry("a"), entry_usable("b", false), entry_usable("c", true)]).await;

    let req = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(req.profile.as_deref(), Some("c"), "health-condemned b skipped before probe");
    // Only c was probed — b was filtered out before phase-2.
    assert_eq!(prober.calls(), 1, "condemned b must not be probed");
    Ok(())
}

/// `RotationConfig::from_env` turns the health gate ON by default and honours
/// `COOP_ROTATE_HEALTH_GATE=0` for rollback.
#[tokio::test]
#[serial_test::serial]
async fn rotation_config_from_env_health_gate_default_on() -> anyhow::Result<()> {
    let prev = std::env::var("COOP_ROTATE_HEALTH_GATE").ok();

    std::env::remove_var("COOP_ROTATE_HEALTH_GATE");
    assert!(RotationConfig::from_env().health_gate, "default ON");

    std::env::set_var("COOP_ROTATE_HEALTH_GATE", "0");
    assert!(!RotationConfig::from_env().health_gate, "=0 disables");

    std::env::set_var("COOP_ROTATE_HEALTH_GATE", "1");
    assert!(RotationConfig::from_env().health_gate, "=1 enables");

    match prev {
        Some(v) => std::env::set_var("COOP_ROTATE_HEALTH_GATE", v),
        None => std::env::remove_var("COOP_ROTATE_HEALTH_GATE"),
    }
    Ok(())
}

/// The `usable` field round-trips through JSON deserialization the way coopmux
/// sends it: present-and-false, present-and-true, and absent (fail-open None).
#[tokio::test]
async fn profile_entry_usable_json_roundtrip() -> anyhow::Result<()> {
    let condemned: ProfileEntry = serde_json::from_value(
        serde_json::json!({"name": "x", "credentials": {}, "usable": false}),
    )?;
    assert_eq!(condemned.usable, Some(false));

    let ok: ProfileEntry = serde_json::from_value(
        serde_json::json!({"name": "x", "credentials": {}, "usable": true}),
    )?;
    assert_eq!(ok.usable, Some(true));

    // Absent field → None (old coopmux body). Fail-open at register time.
    let absent: ProfileEntry =
        serde_json::from_value(serde_json::json!({"name": "x", "credentials": {}}))?;
    assert_eq!(absent.usable, None);

    // And serialization SKIPS the field when None, staying wire-compatible.
    let out = serde_json::to_value(&absent)?;
    assert!(out.get("usable").is_none(), "None usable must not serialize");
    Ok(())
}

// ── Harness gate: Claude and Codex profiles never mix ──────────────────

fn claude_entry(name: &str) -> ProfileEntry {
    ProfileEntry {
        name: name.to_owned(),
        credentials: HashMap::from([("CLAUDE_CODE_OAUTH_TOKEN".to_owned(), format!("tok-{name}"))]),
        usable: None,
    }
}

fn codex_entry(name: &str) -> ProfileEntry {
    ProfileEntry {
        name: name.to_owned(),
        credentials: HashMap::from([(
            "CODEX_AUTH_JSON".to_owned(),
            format!("{{\"n\":\"{name}\"}}"),
        )]),
        usable: None,
    }
}

#[tokio::test]
async fn claude_session_refuses_codex_profiles() {
    let state = ProfileState::new().for_agent(AgentType::Claude);
    state.register(vec![codex_entry("codex-a"), claude_entry("claude-a")]).await;
    state.register(vec![codex_entry("codex-b")]).await;

    let names: Vec<_> = state.list().await.into_iter().map(|p| p.name).collect();
    assert_eq!(names, ["claude-a"]);
    assert_eq!(state.active_name().await.as_deref(), Some("claude-a"));
}

#[tokio::test]
async fn codex_session_refuses_claude_profiles() {
    let state = ProfileState::new().for_agent(AgentType::Codex);
    state.register(vec![claude_entry("claude-a"), codex_entry("codex-a")]).await;

    let names: Vec<_> = state.list().await.into_iter().map(|p| p.name).collect();
    assert_eq!(names, ["codex-a"]);
}

#[tokio::test]
async fn unbound_session_infers_harness_from_active_profile() {
    let state = ProfileState::new();
    state.register(vec![claude_entry("claude-a")]).await;
    state.register(vec![codex_entry("codex-a"), claude_entry("claude-b")]).await;

    let names: Vec<_> = state.list().await.into_iter().map(|p| p.name).collect();
    assert_eq!(names, ["claude-a", "claude-b"]);
}

#[tokio::test]
async fn auto_rotate_never_selects_other_harness_profile() {
    // A foreign profile that got into the set anyway (unbound state seeded by a
    // Codex Active profile, then bound-agnostic): rotation still skips it.
    let state = ProfileState::new();
    state.register(vec![codex_entry("codex-a"), codex_entry("codex-b")]).await;
    {
        // Simulate a pre-gate leftover: push a Claude profile directly.
        let mut profiles = state.profiles.write().await;
        profiles.push(Profile {
            name: "claude-a".into(),
            credentials: claude_entry("claude-a").credentials,
            status: ProfileStatus::Available,
            usable: true,
        });
    }
    // codex-a is Active; claude-a is next in round-robin order after codex-b.
    let first = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(first.profile.as_deref(), Some("codex-b"));
    state.set_active("codex-b").await;

    // Only the Claude profile is left Available: Codex must not rotate onto it.
    match state.try_auto_rotate().await {
        RotateOutcome::Exhausted { .. } => {}
        other => panic!("expected Exhausted (no codex candidate), got {other:?}"),
    }
}

#[tokio::test]
async fn auto_rotate_claude_never_selects_codex_profile() {
    let state = ProfileState::new().for_agent(AgentType::Claude);
    state.register(vec![claude_entry("claude-a"), claude_entry("claude-b")]).await;
    {
        let mut profiles = state.profiles.write().await;
        profiles.push(Profile {
            name: "codex-a".into(),
            credentials: codex_entry("codex-a").credentials,
            status: ProfileStatus::Available,
            usable: true,
        });
    }
    let first = unwrap_switch(state.try_auto_rotate().await);
    assert_eq!(first.profile.as_deref(), Some("claude-b"));
    state.set_active("claude-b").await;
    // claude-a is cooling down; codex-a is the only Available profile.
    match state.try_auto_rotate().await {
        RotateOutcome::Exhausted { .. } => {}
        other => panic!("expected Exhausted (no claude candidate), got {other:?}"),
    }
}
