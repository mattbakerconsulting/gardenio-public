// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Wiring tests for the terminal login-required watchdog in
//! [`process_detected_state`] (ka-0000007394). These exercise the full
//! transition path — `DetectedState` in, `Store` mutation + park out — rather
//! than just the [`LoginRequiredWatchdog`] unit (covered in
//! `driver/error_category_tests.rs`).

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use crate::config::Config;
use crate::driver::{AgentState, DetectedState};
use crate::test_support::StoreBuilder;
use crate::transport::Store;

use super::{process_detected_state, TERMINAL_LOGIN_REQUIRED_PARK_PREFIX};
use crate::session::run::SessionState;

const LOGIN_BANNER: &str = "Not logged in · Please run /login";

fn fresh_session_state() -> SessionState {
    SessionState {
        state_seq: 0,
        last_state: AgentState::Starting,
        idle_since: None,
        idle_timeout: Duration::ZERO,
        pending_switch: None,
        switch_deadline: None,
        drain_deadline: None,
        hard_auth_403: Default::default(),
        login_required: Default::default(),
    }
}

async fn feed_error(store: &Arc<Store>, session: &mut SessionState, config: &Config, detail: &str) {
    let detected = DetectedState {
        state: AgentState::Error { detail: detail.to_owned() },
        tier: 2,
        cause: "test".to_owned(),
    };
    let _ = process_detected_state(store, detected, session, &None, &None, config).await;
}

/// Three consecutive "Not logged in" Error observations park the session
/// terminally and mark it a zombie so the controller / `/livez` recycle the pod.
#[tokio::test]
async fn three_consecutive_not_logged_in_parks_terminally() {
    let config = Config::test();
    let ctx = StoreBuilder::new().build();
    let mut session = fresh_session_state();

    // First two are under the threshold → no terminal park, not a zombie yet.
    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    assert!(
        !ctx.store.lifecycle.zombie.load(Ordering::Acquire),
        "must not be a zombie before the threshold"
    );
    assert_eq!(session.login_required.consecutive(), 2);

    // Third trips the watchdog → terminal park + zombie.
    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;

    assert!(
        ctx.store.lifecycle.zombie.load(Ordering::Acquire),
        "third consecutive not-logged-in must mark zombie"
    );
    let state = ctx.store.driver.agent_state.read().await.clone();
    match state {
        AgentState::Parked { reason, .. } => {
            assert!(
                reason.starts_with(TERMINAL_LOGIN_REQUIRED_PARK_PREFIX),
                "parked_reason should carry the terminal prefix, got: {reason}"
            );
        }
        other => panic!("expected terminal Parked state, got {other:?}"),
    }
}

/// A transient not-logged-in cleared by a productive turn must NOT park — the
/// 25/26-recover credential-switch calibration (ka-0000006636). Mirrors the
/// inline Working→Idle recovery path.
#[tokio::test]
async fn transient_not_logged_in_then_recovery_does_not_park() {
    let config = Config::test();
    let ctx = StoreBuilder::new().build();
    let mut session = fresh_session_state();

    // Two not-logged-in observations (under threshold).
    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    assert_eq!(session.login_required.consecutive(), 2);

    // A productive turn: Working → Idle with no error trailer. This resets the
    // login watchdog via the inline-recovery branch.
    session.last_state = AgentState::Working;
    let working = DetectedState { state: AgentState::Working, tier: 2, cause: "test".to_owned() };
    let _ = process_detected_state(&ctx.store, working, &mut session, &None, &None, &config).await;
    *ctx.store.driver.last_message.write().await = None;
    let idle = DetectedState { state: AgentState::Idle, tier: 2, cause: "test".to_owned() };
    let _ = process_detected_state(&ctx.store, idle, &mut session, &None, &None, &config).await;

    assert_eq!(session.login_required.consecutive(), 0, "productive turn must reset the counter");

    // One more not-logged-in after recovery is still under threshold → no park.
    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    assert!(
        !ctx.store.lifecycle.zombie.load(Ordering::Acquire),
        "a single banner after recovery must not park"
    );
    assert!(!matches!(*ctx.store.driver.agent_state.read().await, AgentState::Parked { .. }));
}

/// A non-login error between banners resets the run, so an interleaved
/// auth-failure storm does not spuriously trip the terminal login park.
#[tokio::test]
async fn interleaved_other_error_resets_login_run() {
    let config = Config::test();
    let ctx = StoreBuilder::new().build();
    let mut session = fresh_session_state();

    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    feed_error(&ctx.store, &mut session, &config, "API Error: 500 Internal server error").await;
    assert_eq!(session.login_required.consecutive(), 0, "a non-login error resets the run");

    // Two more banners: still only 2 consecutive → under the threshold.
    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    feed_error(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    assert!(
        !ctx.store.lifecycle.zombie.load(Ordering::Acquire),
        "interleaved sequence must not reach the terminal park"
    );
}

/// Simulate one inline Working→Idle turn whose trailing assistant message is
/// `msg` — the Tier-2 log shape where the banner rides in `last_message` rather
/// than a distinct Error state.
async fn feed_inline_turn(
    store: &Arc<Store>,
    session: &mut SessionState,
    config: &Config,
    msg: &str,
) {
    session.last_state = AgentState::Working;
    let working = DetectedState { state: AgentState::Working, tier: 2, cause: "test".to_owned() };
    let _ = process_detected_state(store, working, session, &None, &None, config).await;
    *store.driver.last_message.write().await = Some(msg.to_owned());
    let idle = DetectedState { state: AgentState::Idle, tier: 2, cause: "test".to_owned() };
    let _ = process_detected_state(store, idle, session, &None, &None, config).await;
}

/// Cross-watchdog reset on the inline path (review finding): alternating inline
/// login-required and hard-auth-403 turns must reset each other and trip
/// NEITHER terminal park — identical semantics to the Error-state path. Before
/// the fix the mutually-exclusive inline guards only fed the matching watchdog,
/// so a strict alternation could drift one counter to the threshold.
#[tokio::test]
async fn inline_alternating_403_and_login_trips_neither() {
    let config = Config::test();
    let ctx = StoreBuilder::new().build();
    let mut session = fresh_session_state();

    let h403 = "403 key not allowed to access model";
    // login, 403, login, 403, login — 5 turns, strict alternation.
    feed_inline_turn(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    feed_inline_turn(&ctx.store, &mut session, &config, h403).await;
    feed_inline_turn(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    feed_inline_turn(&ctx.store, &mut session, &config, h403).await;
    feed_inline_turn(&ctx.store, &mut session, &config, LOGIN_BANNER).await;

    assert_eq!(session.login_required.consecutive(), 1, "403 turns must reset the login streak");
    assert_eq!(session.hard_auth_403.consecutive(), 0, "a login turn must reset the 403 streak");
    assert!(
        !ctx.store.lifecycle.zombie.load(Ordering::Acquire),
        "alternating inline auth errors must not reach any terminal park"
    );
    assert!(!matches!(*ctx.store.driver.agent_state.read().await, AgentState::Parked { .. }));
}

/// Three CONSECUTIVE inline login-required turns (no intervening reset) still
/// trip the terminal park — the cross-reset fix must not weaken real detection.
#[tokio::test]
async fn inline_three_consecutive_login_parks() {
    let config = Config::test();
    let ctx = StoreBuilder::new().build();
    let mut session = fresh_session_state();

    feed_inline_turn(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    feed_inline_turn(&ctx.store, &mut session, &config, LOGIN_BANNER).await;
    feed_inline_turn(&ctx.store, &mut session, &config, LOGIN_BANNER).await;

    assert!(
        ctx.store.lifecycle.zombie.load(Ordering::Acquire),
        "three consecutive inline login turns must park terminally"
    );
    let state = ctx.store.driver.agent_state.read().await.clone();
    match state {
        AgentState::Parked { reason, .. } => {
            assert!(reason.starts_with(TERMINAL_LOGIN_REQUIRED_PARK_PREFIX), "got: {reason}");
        }
        other => panic!("expected terminal Parked, got {other:?}"),
    }
}
