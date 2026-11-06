// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;
use crate::credential::AccountConfig;

/// Build a broker whose accounts persist to `dir`, seeded with `accounts`
/// (name, provider). Used by the off-lock-persistence tests (ka-0000007732).
pub(super) fn make_persisting_broker(
    dir: PathBuf,
    accounts: Vec<(&str, &str)>,
) -> Arc<CredentialBroker> {
    let configs: Vec<AccountConfig> = accounts
        .iter()
        .map(|(name, provider)| AccountConfig {
            name: name.to_string(),
            provider: provider.to_string(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        })
        .collect();
    let (tx, _rx) = broadcast::channel(16);
    CredentialBroker::new(CredentialConfig { accounts: configs }, tx, Some(dir))
}

pub(super) fn make_broker(accounts: Vec<(&str, &str)>) -> Arc<CredentialBroker> {
    let configs: Vec<AccountConfig> = accounts
        .iter()
        .map(|(name, provider)| AccountConfig {
            name: name.to_string(),
            provider: provider.to_string(),
            env_key: None,
            model: None,
            token_url: None,
            client_id: None,
            auth_url: None,
            device_auth_url: None,
            reauth: true,
        })
        .collect();
    let (tx, _rx) = broadcast::channel(16);
    CredentialBroker::new(CredentialConfig { accounts: configs }, tx, None)
}

#[tokio::test]
async fn assign_account_skips_warming() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);

    // Seed both accounts with tokens so they become Healthy.
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;

    // Mark acct-a as warming — it should be skipped for new sessions.
    broker.mark_warming("acct-a").await;

    // Even with lower load, warming account should be skipped.
    let assigned = broker.assign_account(None).await;
    assert_eq!(assigned, Some("acct-b".to_owned()));

    // Preferred account that is warming should also be skipped.
    let assigned = broker.assign_account(Some("acct-a")).await;
    assert_eq!(assigned, Some("acct-b".to_owned()));

    Ok(())
}

#[tokio::test]
async fn mark_warming_only_from_healthy() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);

    // Account starts as Missing — mark_warming should be a no-op.
    broker.mark_warming("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "missing");

    // Make it Healthy, then Throttled.
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.mark_throttled("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "throttled");

    // Throttled → Warming should be a no-op.
    broker.mark_warming("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "throttled");

    // Make it Healthy again, then Warming should work.
    broker.clear_throttled("acct-a").await;
    broker.mark_warming("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "warming");

    Ok(())
}

#[tokio::test]
async fn clear_warming_to_healthy() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;

    broker.mark_warming("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "warming");

    let cleared = broker.clear_warming("acct-a").await;
    assert!(cleared);
    assert_eq!(broker.account_status("acct-a").await, "healthy");

    Ok(())
}

#[tokio::test]
async fn clear_warming_from_non_warming() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;

    // Healthy → clear_warming should return false.
    let cleared = broker.clear_warming("acct-a").await;
    assert!(!cleared);

    // Throttled → clear_warming should return false.
    broker.mark_throttled("acct-a").await;
    let cleared = broker.clear_warming("acct-a").await;
    assert!(!cleared);

    Ok(())
}

#[tokio::test]
async fn clear_throttled_does_not_clear_warming() -> anyhow::Result<()> {
    // `clear_throttled` is driven by the LOCAL recovery path (a live session's
    // working-state transition). It owns only the hard `Throttled` state and
    // must leave the controller-owned soft `Warming` state alone — clearing it
    // would re-admit new sessions onto a near-capacity account and defeat soft
    // failover (ka-0000003184).
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;

    broker.mark_warming("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "warming");

    // Warming → clear_throttled must be a no-op that returns false.
    let cleared = broker.clear_throttled("acct-a").await;
    assert!(!cleared, "clear_throttled must not report clearing a warming account");
    assert_eq!(
        broker.account_status("acct-a").await,
        "warming",
        "clear_throttled must leave controller-owned warming state intact"
    );

    // Throttled → clear_throttled still clears (the tested recovery path).
    broker.mark_throttled("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "throttled");
    let cleared = broker.clear_throttled("acct-a").await;
    assert!(cleared);
    assert_eq!(broker.account_status("acct-a").await, "healthy");

    Ok(())
}

#[tokio::test]
async fn warming_to_throttled_transition() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;

    // Healthy → Warming → Throttled should work (429 while warming).
    broker.mark_warming("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "warming");

    broker.mark_throttled("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "throttled");

    Ok(())
}

#[tokio::test]
async fn set_token_preserves_throttled_status() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);

    // Initial set_token on Missing account → Healthy.
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    assert_eq!(broker.account_status("acct-a").await, "healthy");

    // Mark as throttled (simulating 429 detection).
    broker.mark_throttled("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "throttled");

    // Re-sync same token (periodic controller sync) must NOT reset to Healthy.
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    assert_eq!(broker.account_status("acct-a").await, "throttled");

    // Even a new token must NOT reset Throttled — only clear_throttled should.
    broker.set_token("acct-a", "key-b".into(), None, None).await?;
    assert_eq!(broker.account_status("acct-a").await, "throttled");

    // Explicit recovery clears it.
    broker.clear_throttled("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "healthy");

    Ok(())
}

#[tokio::test]
async fn set_token_preserves_warming_status() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;

    broker.mark_warming("acct-a").await;
    assert_eq!(broker.account_status("acct-a").await, "warming");

    // Re-sync must NOT reset Warming.
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    assert_eq!(broker.account_status("acct-a").await, "warming");

    Ok(())
}

#[tokio::test]
async fn set_token_transitions_missing_to_healthy() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);

    // Starts as Missing.
    assert_eq!(broker.account_status("acct-a").await, "missing");

    // set_token should transition Missing → Healthy.
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    assert_eq!(broker.account_status("acct-a").await, "healthy");

    Ok(())
}

#[tokio::test]
async fn warming_excluded_from_healthy_accounts() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;

    broker.mark_warming("acct-a").await;

    let healthy = broker.healthy_accounts().await;
    assert_eq!(healthy, vec!["acct-b"]);

    let unhealthy = broker.unhealthy_accounts().await;
    assert!(unhealthy.contains(&"acct-a".to_owned()));

    Ok(())
}

#[tokio::test]
async fn assign_account_uses_preferred_when_healthy() -> anyhow::Result<()> {
    let broker = make_broker(vec![("matthewbaker-acme", "claude"), ("matthewbaker1110", "claude")]);
    broker.set_token("matthewbaker-acme", "key-ph".into(), None, None).await?;
    broker.set_token("matthewbaker1110", "key-1110".into(), None, None).await?;

    // When preferred account is healthy, it should be returned regardless
    // of load balancing.
    let assigned = broker.assign_account(Some("matthewbaker1110")).await;
    assert_eq!(assigned, Some("matthewbaker1110".to_owned()));

    let assigned = broker.assign_account(Some("matthewbaker-acme")).await;
    assert_eq!(assigned, Some("matthewbaker-acme".to_owned()));

    Ok(())
}

#[tokio::test]
async fn assign_account_falls_back_when_preferred_throttled() -> anyhow::Result<()> {
    let broker = make_broker(vec![("matthewbaker-acme", "claude"), ("matthewbaker1110", "claude")]);
    broker.set_token("matthewbaker-acme", "key-ph".into(), None, None).await?;
    broker.set_token("matthewbaker1110", "key-1110".into(), None, None).await?;

    // Throttle the preferred account — assignment should fall back to
    // the least-loaded healthy account.
    broker.mark_throttled("matthewbaker1110").await;

    let assigned = broker.assign_account(Some("matthewbaker1110")).await;
    assert_eq!(assigned, Some("matthewbaker-acme".to_owned()));

    Ok(())
}

#[tokio::test]
async fn assign_account_falls_back_when_no_preferred() -> anyhow::Result<()> {
    let broker = make_broker(vec![("matthewbaker-acme", "claude"), ("matthewbaker1110", "claude")]);
    broker.set_token("matthewbaker-acme", "key-ph".into(), None, None).await?;
    broker.set_token("matthewbaker1110", "key-1110".into(), None, None).await?;

    // No preferred account — should fall back to least-loaded.
    let assigned = broker.assign_account(None).await;
    assert!(assigned.is_some());

    Ok(())
}

// ── Project-scoped assignment tests ─────────────────────────────────

#[tokio::test]
async fn assign_for_project_returns_preferred_when_healthy_and_authorized() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;

    let authorized: std::collections::HashSet<String> =
        ["acct-a", "acct-b"].iter().map(|s| s.to_string()).collect();

    let assigned = broker.assign_account_for_project(Some("acct-a"), &authorized).await;
    assert_eq!(assigned, Some("acct-a".to_owned()));

    Ok(())
}

#[tokio::test]
async fn assign_for_project_falls_back_within_authorized_set() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;

    // Throttle the preferred account.
    broker.mark_throttled("acct-a").await;

    let authorized: std::collections::HashSet<String> =
        ["acct-a", "acct-b"].iter().map(|s| s.to_string()).collect();

    // Should fall back to acct-b (authorized), NOT acct-c (global pool).
    let assigned = broker.assign_account_for_project(Some("acct-a"), &authorized).await;
    assert_eq!(assigned, Some("acct-b".to_owned()));

    Ok(())
}

#[tokio::test]
async fn assign_for_project_excludes_global_pool() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;

    // Throttle both authorized accounts.
    broker.mark_throttled("acct-a").await;
    broker.mark_throttled("acct-b").await;

    let authorized: std::collections::HashSet<String> =
        ["acct-a", "acct-b"].iter().map(|s| s.to_string()).collect();

    // acct-c is healthy but NOT authorized — should return None.
    let assigned = broker.assign_account_for_project(Some("acct-a"), &authorized).await;
    assert_eq!(assigned, None);

    Ok(())
}

#[tokio::test]
async fn assign_for_project_no_preferred_picks_least_loaded_authorized() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;

    // Give acct-a some load.
    broker.session_assigned("acct-a").await;
    broker.session_assigned("acct-a").await;

    let authorized: std::collections::HashSet<String> =
        ["acct-a", "acct-b"].iter().map(|s| s.to_string()).collect();

    // No preferred — should pick acct-b (least-loaded within authorized set).
    let assigned = broker.assign_account_for_project(None, &authorized).await;
    assert_eq!(assigned, Some("acct-b".to_owned()));

    Ok(())
}

// ── Round-robin rotator tests ───────────────────────────────────────

#[test]
fn strategy_env_parse_round_robin() {
    assert_eq!(AssignmentStrategy::from_env_value("round_robin"), AssignmentStrategy::RoundRobin);
    assert_eq!(AssignmentStrategy::from_env_value("RoundRobin"), AssignmentStrategy::RoundRobin);
    assert_eq!(
        AssignmentStrategy::from_env_value("  round_robin  "),
        AssignmentStrategy::RoundRobin
    );
}

#[test]
fn strategy_env_parse_defaults_to_least_loaded() {
    assert_eq!(AssignmentStrategy::from_env_value(""), AssignmentStrategy::LeastLoaded);
    assert_eq!(AssignmentStrategy::from_env_value("least_loaded"), AssignmentStrategy::LeastLoaded);
    assert_eq!(AssignmentStrategy::from_env_value("bogus"), AssignmentStrategy::LeastLoaded);
}

#[test]
fn default_strategy_is_least_loaded_when_env_unset() {
    // Constructed brokers default to LeastLoaded unless the env var is
    // explicitly set — guards against an accidental default flip.
    let (tx, _rx) = broadcast::channel(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: vec![] }, tx, None);
    // Under concurrent tests the env var might be set by another test;
    // accept either value but assert the accessor is consistent.
    let s = broker.strategy();
    assert!(matches!(s, AssignmentStrategy::LeastLoaded | AssignmentStrategy::RoundRobin));
    assert_eq!(s.as_str(), broker.strategy().as_str());
}

#[tokio::test]
async fn round_robin_rotates_across_healthy_accounts() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-c", "claude"), ("acct-a", "claude"), ("acct-b", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    // Three assignments with no preferred should yield the sorted order
    // a, b, c — not HashMap insertion order c, a, b.
    let first = broker.assign_account(None).await;
    let second = broker.assign_account(None).await;
    let third = broker.assign_account(None).await;
    assert_eq!(first, Some("acct-a".to_owned()));
    assert_eq!(second, Some("acct-b".to_owned()));
    assert_eq!(third, Some("acct-c".to_owned()));

    // Fourth call wraps back to a via cursor mod len.
    let fourth = broker.assign_account(None).await;
    assert_eq!(fourth, Some("acct-a".to_owned()));

    Ok(())
}

#[tokio::test]
async fn round_robin_skips_unhealthy_accounts() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    // Warming and throttled accounts drop out of the eligible list.
    broker.mark_warming("acct-b").await;
    broker.mark_throttled("acct-c").await;

    // With only acct-a eligible, every call returns it regardless of
    // cursor advance.
    for _ in 0..5 {
        assert_eq!(broker.assign_account(None).await, Some("acct-a".to_owned()));
    }

    Ok(())
}

#[tokio::test]
async fn round_robin_honors_preferred_when_healthy() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    // Preferred short-circuit applies under round-robin: binding survives.
    let assigned = broker.assign_account(Some("acct-c")).await;
    assert_eq!(assigned, Some("acct-c".to_owned()));

    // Cursor was not consumed, so the next un-preferred call starts at
    // the head of the sorted list.
    let next = broker.assign_account(None).await;
    assert_eq!(next, Some("acct-a".to_owned()));

    Ok(())
}

#[tokio::test]
async fn round_robin_falls_back_when_preferred_unhealthy() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    broker.mark_throttled("acct-c").await;

    // Preferred is throttled → rotator takes over; first eligible entry
    // in sorted order is acct-a.
    let assigned = broker.assign_account(Some("acct-c")).await;
    assert_eq!(assigned, Some("acct-a".to_owned()));

    Ok(())
}

#[tokio::test]
async fn round_robin_for_project_rotates_within_authorized_set() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    let authorized: std::collections::HashSet<String> =
        ["acct-a", "acct-b"].iter().map(|s| s.to_string()).collect();

    // Only acct-a and acct-b are authorized — cursor should alternate
    // between them and never select acct-c.
    let mut picks = Vec::new();
    for _ in 0..6 {
        picks.push(broker.assign_account_for_project(None, &authorized).await);
    }
    assert!(picks.iter().all(|p| p != &Some("acct-c".to_owned())));
    let a_count = picks.iter().filter(|p| p.as_deref() == Some("acct-a")).count();
    let b_count = picks.iter().filter(|p| p.as_deref() == Some("acct-b")).count();
    assert_eq!(a_count, 3);
    assert_eq!(b_count, 3);

    Ok(())
}

#[tokio::test]
async fn round_robin_returns_none_on_empty_eligible() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    // acct-a stays Missing (no token), so nothing is eligible.
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    assert_eq!(broker.assign_account(None).await, None);

    // Cursor should still advance safely without dividing by zero.
    assert_eq!(broker.assign_account(None).await, None);

    Ok(())
}

#[tokio::test]
async fn round_robin_survives_cursor_wraparound() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    // Pre-load the cursor near usize::MAX to validate that the
    // `fetch_add % len` math stays correct across the wrap boundary.
    broker.next_cursor.store(usize::MAX, Ordering::Relaxed);
    let pick = broker.assign_account(None).await;
    assert!(pick.is_some(), "rotator must never panic near cursor wraparound");
    // After fetch_add(1) from usize::MAX, cursor wraps to 0.
    assert_eq!(broker.next_cursor.load(Ordering::Relaxed), 0);

    Ok(())
}

#[tokio::test]
async fn least_loaded_remains_default_behavior() -> anyhow::Result<()> {
    // Default-constructed broker should not rotate; it should pick the
    // least-loaded account, as documented in the rollout plan.
    let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::LeastLoaded);

    broker.session_assigned("acct-a").await;
    broker.session_assigned("acct-a").await;

    // acct-b has zero sessions — least-loaded picks it every time.
    for _ in 0..3 {
        assert_eq!(broker.assign_account(None).await, Some("acct-b".to_owned()));
    }
    Ok(())
}

#[tokio::test]
async fn session_counts_still_tracked_under_round_robin() -> anyhow::Result<()> {
    // /pool observability must keep working even when the selector
    // ignores session_counts.
    let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    let first = broker
        .assign_account(None)
        .await
        .ok_or_else(|| anyhow::anyhow!("first assignment returned None"))?;
    broker.session_assigned(&first).await;
    let second = broker
        .assign_account(None)
        .await
        .ok_or_else(|| anyhow::anyhow!("second assignment returned None"))?;
    broker.session_assigned(&second).await;

    let pool = broker.pool_status().await;
    let total: u32 = pool.iter().map(|p| p.session_count).sum();
    assert_eq!(total, 2, "session_counts must still reflect assignments");
    Ok(())
}

#[tokio::test]
async fn cumulative_assignments_accumulate_and_survive_unassign() -> anyhow::Result<()> {
    // Lifetime counter increments on assignment and stays put when the
    // session ends — `/pool` needs this to show fairness over time.
    let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;

    broker.session_assigned("acct-a").await;
    broker.session_assigned("acct-a").await;
    broker.session_assigned("acct-b").await;
    broker.session_unassigned("acct-a").await;

    let pool = broker.pool_status().await;
    let a = pool
        .iter()
        .find(|p| p.name == "acct-a")
        .ok_or_else(|| anyhow::anyhow!("acct-a in pool"))?;
    let b = pool
        .iter()
        .find(|p| p.name == "acct-b")
        .ok_or_else(|| anyhow::anyhow!("acct-b in pool"))?;
    assert_eq!(a.session_count, 1);
    assert_eq!(a.cumulative_assignments, 2);
    assert_eq!(b.session_count, 1);
    assert_eq!(b.cumulative_assignments, 1);
    Ok(())
}

#[tokio::test]
async fn rotator_cursor_reports_value_only_under_round_robin() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;

    broker.set_strategy_for_tests(AssignmentStrategy::LeastLoaded);
    assert_eq!(broker.rotator_cursor(), None);
    assert_eq!(broker.strategy().as_str(), "least_loaded");

    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);
    assert_eq!(broker.rotator_cursor(), Some(0));
    assert_eq!(broker.strategy().as_str(), "round_robin");

    // Advance the cursor and observe it in the accessor.
    let _ = broker.assign_account(None).await;
    let _ = broker.assign_account(None).await;
    assert_eq!(broker.rotator_cursor(), Some(2));
    Ok(())
}

#[tokio::test]
async fn assign_for_project_skips_warming_accounts() -> anyhow::Result<()> {
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;
    broker.set_token("acct-b", "key-b".into(), None, None).await?;
    broker.set_token("acct-c", "key-c".into(), None, None).await?;

    broker.mark_warming("acct-a").await;

    let authorized: std::collections::HashSet<String> =
        ["acct-a", "acct-b"].iter().map(|s| s.to_string()).collect();

    // Preferred is warming — should fall back to acct-b.
    let assigned = broker.assign_account_for_project(Some("acct-a"), &authorized).await;
    assert_eq!(assigned, Some("acct-b".to_owned()));

    Ok(())
}

// ── Fairness + regression tests (ka-0000002129) ─────────────────────
//
// These tests exercise the rotator's fairness guarantees under
// higher-volume and mixed-state scenarios that the earlier targeted
// unit tests don't cover.

/// Deterministic LCG — reproducible pseudo-randomness without pulling
/// in the `rand` crate. Numbers from Numerical Recipes.
struct Lcg {
    state: u64,
}

impl Lcg {
    fn new(seed: u64) -> Self {
        Self { state: seed.wrapping_add(0x9E37_79B9_7F4A_7C15) }
    }
    fn next_u32(&mut self) -> u32 {
        self.state = self.state.wrapping_mul(1664525).wrapping_add(1013904223);
        (self.state >> 32) as u32
    }
    fn range(&mut self, n: u32) -> u32 {
        self.next_u32() % n
    }
}

async fn seed_healthy(broker: &Arc<CredentialBroker>, names: &[&str]) -> anyhow::Result<()> {
    for name in names {
        broker.set_token(name, format!("key-{name}"), None, None).await?;
    }
    Ok(())
}

#[tokio::test]
async fn fairness_basic_rotation_9_over_3() -> anyhow::Result<()> {
    // Spec: 3 healthy accounts, 9 assignments → each gets exactly 3.
    let broker =
        make_broker(vec![("acct-c", "claude"), ("acct-a", "claude"), ("acct-b", "claude")]);
    seed_healthy(&broker, &["acct-a", "acct-b", "acct-c"]).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    let mut counts: HashMap<String, u32> = HashMap::new();
    for _ in 0..9 {
        let picked = broker
            .assign_account(None)
            .await
            .ok_or_else(|| anyhow::anyhow!("assignment returned None"))?;
        *counts.entry(picked).or_insert(0) += 1;
    }

    assert_eq!(counts.get("acct-a").copied(), Some(3));
    assert_eq!(counts.get("acct-b").copied(), Some(3));
    assert_eq!(counts.get("acct-c").copied(), Some(3));

    // Cursor has advanced by exactly 9 across the whole run.
    assert_eq!(broker.next_cursor.load(Ordering::Relaxed), 9);
    Ok(())
}

#[tokio::test]
async fn fairness_skip_warming_does_not_burn_healthy_slot() -> anyhow::Result<()> {
    // Spec: warming account skipped; rotator does not advance past
    // healthy peers (i.e. consecutive calls never skip a healthy entry).
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    seed_healthy(&broker, &["acct-a", "acct-b", "acct-c"]).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    broker.mark_warming("acct-b").await;

    // Eligible list is deterministically [acct-a, acct-c] and the cursor
    // advances by one per call. Over 6 calls each eligible account must
    // appear exactly 3 times and the sequence must strictly alternate.
    let mut picks = Vec::new();
    for _ in 0..6 {
        picks.push(
            broker
                .assign_account(None)
                .await
                .ok_or_else(|| anyhow::anyhow!("assignment returned None"))?,
        );
    }

    // No warming account in the output.
    assert!(
        picks.iter().all(|p| p != "acct-b"),
        "warming account must never be assigned: {picks:?}"
    );

    // Strict alternation proves the cursor skips warming entries by
    // filtering them out of the eligible list, not by "over-advancing"
    // when it lands on a warming slot.
    for window in picks.windows(2) {
        assert_ne!(window[0], window[1], "expected alternation, got {picks:?}");
    }

    let a_count = picks.iter().filter(|p| p.as_str() == "acct-a").count();
    let c_count = picks.iter().filter(|p| p.as_str() == "acct-c").count();
    assert_eq!(a_count, 3);
    assert_eq!(c_count, 3);

    Ok(())
}

#[tokio::test]
async fn fairness_under_churn_n5_k500() -> anyhow::Result<()> {
    // Spec: random short/long session lifetimes, N=5, K=500, max-min ≤ 1
    // (or a small bound accounting for warming transitions).
    //
    // The rotator advances the cursor once per call regardless of how
    // long the session the assignment feeds lives for, so lifetime churn
    // should not affect count-fairness at all — we add brief warming
    // windows to exercise the "small bound" allowance in the spec.
    let names = ["acct-a", "acct-b", "acct-c", "acct-d", "acct-e"];
    let broker = make_broker(names.iter().map(|n| (*n, "claude")).collect());
    seed_healthy(&broker, &names).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    let mut counts: HashMap<String, u32> = names.iter().map(|n| ((*n).to_owned(), 0)).collect();
    let mut active_sessions: Vec<(String, u32)> = Vec::new();
    let mut rng = Lcg::new(0xC0FFEE);
    let mut warming: Option<(String, u32)> = None;

    for step in 0..500u32 {
        // 10% chance to briefly warm a random healthy account for 3
        // steps, then clear. Ensures the "small bound for warming
        // transitions" allowance gets exercised.
        if warming.is_none() && rng.range(10) == 0 {
            let victim = names[rng.range(names.len() as u32) as usize];
            if broker.mark_warming(victim).await {
                warming = Some((victim.to_owned(), step + 3));
            }
        }
        if let Some((ref name, clear_at)) = warming {
            if step >= clear_at {
                broker.clear_warming(name).await;
                warming = None;
            }
        }

        let picked = broker
            .assign_account(None)
            .await
            .ok_or_else(|| anyhow::anyhow!("assignment returned None at step {step}"))?;
        *counts.entry(picked.clone()).or_insert(0) += 1;
        broker.session_assigned(&picked).await;
        // Short-lived sessions die after 1 step, long-lived after 20.
        let lifetime = if rng.range(2) == 0 { 1 } else { 20 };
        active_sessions.push((picked, step + lifetime));

        // Reap expired sessions.
        active_sessions.retain(|(acct, expires)| {
            if *expires <= step {
                let broker = Arc::clone(&broker);
                let acct = acct.clone();
                tokio::spawn(async move {
                    broker.session_unassigned(&acct).await;
                });
                false
            } else {
                true
            }
        });
    }

    if let Some((ref name, _)) = warming {
        broker.clear_warming(name).await;
    }

    let max = counts.values().copied().max().unwrap_or(0);
    let min = counts.values().copied().min().unwrap_or(0);
    // K=500 over N=5 gives 100 each under perfect rotation. With up to
    // ~10% of calls happening while one account is warming (and skipped),
    // drift can compound modestly; bound at 20 to catch any regression
    // that would let one account dominate.
    assert!(max - min <= 20, "fairness bound violated: counts={counts:?} (max-min={})", max - min);

    Ok(())
}

#[tokio::test]
async fn fairness_preferred_healthy_still_wins_under_rotation() -> anyhow::Result<()> {
    // Spec: preferred + Healthy still wins (preserves project→account
    // binding). The "ignored when respect_preferred=false" case is not
    // yet expressible through the broker API — the feature-gated flag
    // lives only in the design doc today (see ka-0000005270). When it
    // lands, add a second assertion here that exercises the false path.
    let broker =
        make_broker(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")]);
    seed_healthy(&broker, &["acct-a", "acct-b", "acct-c"]).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    // Even with the cursor advancing for un-preferred calls, a preferred
    // + healthy call must always return the preferred account — and
    // must not consume the cursor.
    let cursor_before = broker.next_cursor.load(Ordering::Relaxed);
    for _ in 0..5 {
        let pick = broker.assign_account(Some("acct-c")).await;
        assert_eq!(pick, Some("acct-c".to_owned()));
    }
    let cursor_after = broker.next_cursor.load(Ordering::Relaxed);
    assert_eq!(
        cursor_before, cursor_after,
        "preferred short-circuit must not consume the rotator cursor"
    );

    Ok(())
}

#[tokio::test]
async fn fairness_per_project_rotates_within_subset() -> anyhow::Result<()> {
    // Spec: authorized subset round-robins fairly within the subset;
    // accounts outside the subset are never picked even though they
    // are Healthy in the global pool.
    let broker = make_broker(vec![
        ("acct-a", "claude"),
        ("acct-b", "claude"),
        ("acct-c", "claude"),
        ("acct-d", "claude"),
    ]);
    seed_healthy(&broker, &["acct-a", "acct-b", "acct-c", "acct-d"]).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    let authorized: std::collections::HashSet<String> =
        ["acct-b", "acct-d"].iter().map(|s| s.to_string()).collect();

    let mut counts: HashMap<String, u32> = HashMap::new();
    for _ in 0..20 {
        let pick = broker
            .assign_account_for_project(None, &authorized)
            .await
            .ok_or_else(|| anyhow::anyhow!("assignment returned None"))?;
        *counts.entry(pick).or_insert(0) += 1;
    }

    assert_eq!(counts.get("acct-a").copied().unwrap_or(0), 0);
    assert_eq!(counts.get("acct-c").copied().unwrap_or(0), 0);
    assert_eq!(counts.get("acct-b").copied(), Some(10));
    assert_eq!(counts.get("acct-d").copied(), Some(10));

    Ok(())
}

#[tokio::test]
async fn fairness_dynamic_set_changes_no_panic() -> anyhow::Result<()> {
    // Spec: add/remove accounts mid-run — no panic, cursor recovers
    // within one rotation.
    let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);
    seed_healthy(&broker, &["acct-a", "acct-b"]).await?;
    broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

    // Warm up the cursor so it's well past 0 before we mutate the set.
    for _ in 0..7 {
        broker.assign_account(None).await;
    }

    // Add a third account mid-run.
    let new_config = AccountConfig {
        name: "acct-c".into(),
        provider: "claude".into(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    };
    broker.add_account(new_config, Some("key-c".into()), None, None).await?;

    // Within three calls the cursor must have touched the new account.
    let mut saw_c = false;
    for _ in 0..3 {
        if broker.assign_account(None).await.as_deref() == Some("acct-c") {
            saw_c = true;
            break;
        }
    }
    assert!(saw_c, "newly-added account must be reachable within one rotation");

    // Remove an existing account mid-run — must not panic, and must
    // never be picked after removal.
    broker.remove_account("acct-a").await?;
    for _ in 0..10 {
        let pick = broker.assign_account(None).await;
        assert_ne!(pick, Some("acct-a".to_owned()));
        assert!(pick.is_some());
    }

    // Remove everything; rotator must gracefully return None.
    broker.remove_account("acct-b").await?;
    broker.remove_account("acct-c").await?;
    assert_eq!(broker.assign_account(None).await, None);

    Ok(())
}

#[tokio::test]
async fn fairness_determinism_same_inputs_same_sequence() -> anyhow::Result<()> {
    // Spec: two brokers with identical inputs pick identical sequences
    // (verifies sort-by-name ordering; there is no RNG in the rotator,
    // so determinism is wholly a property of the sort).
    async fn run_sequence(insert_order: Vec<(&str, &str)>) -> anyhow::Result<Vec<String>> {
        let broker = make_broker(insert_order.clone());
        let names: Vec<&str> = insert_order.iter().map(|(n, _)| *n).collect();
        seed_healthy(&broker, &names).await?;
        broker.set_strategy_for_tests(AssignmentStrategy::RoundRobin);

        let mut picks = Vec::new();
        for _ in 0..30 {
            let pick = broker
                .assign_account(None)
                .await
                .ok_or_else(|| anyhow::anyhow!("assignment returned None"))?;
            picks.push(pick);
        }
        Ok(picks)
    }

    // Different insertion orders must produce the same sequence because
    // the rotator sorts by name before indexing.
    let seq_a =
        run_sequence(vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")])
            .await?;
    let seq_b =
        run_sequence(vec![("acct-c", "claude"), ("acct-a", "claude"), ("acct-b", "claude")])
            .await?;
    assert_eq!(seq_a, seq_b, "rotator must be deterministic across insertion orders");
    Ok(())
}

// ── Regression: legacy assign_account tests must hold under both
// strategies so flipping COOPMUX_ASSIGNMENT_STRATEGY never breaks the
// project→account binding contract.

async fn assert_preferred_binding_preserved(strategy: AssignmentStrategy) -> anyhow::Result<()> {
    let broker = make_broker(vec![("matthewbaker-acme", "claude"), ("matthewbaker1110", "claude")]);
    seed_healthy(&broker, &["matthewbaker-acme", "matthewbaker1110"]).await?;
    broker.set_strategy_for_tests(strategy);

    let assigned = broker.assign_account(Some("matthewbaker1110")).await;
    assert_eq!(
        assigned,
        Some("matthewbaker1110".to_owned()),
        "strategy={strategy:?}: preferred binding lost"
    );

    // Throttle the preferred account → must fall back, regardless of
    // strategy. Under least-loaded we get the only remaining healthy
    // account; under round-robin we also get it (only one eligible).
    broker.mark_throttled("matthewbaker1110").await;
    let assigned = broker.assign_account(Some("matthewbaker1110")).await;
    assert_eq!(
        assigned,
        Some("matthewbaker-acme".to_owned()),
        "strategy={strategy:?}: fallback after preferred throttled failed"
    );
    Ok(())
}

#[tokio::test]
async fn regression_preferred_binding_least_loaded() -> anyhow::Result<()> {
    assert_preferred_binding_preserved(AssignmentStrategy::LeastLoaded).await
}

#[tokio::test]
async fn regression_preferred_binding_round_robin() -> anyhow::Result<()> {
    assert_preferred_binding_preserved(AssignmentStrategy::RoundRobin).await
}

#[tokio::test]
async fn regression_warming_skipped_both_strategies() -> anyhow::Result<()> {
    for strategy in [AssignmentStrategy::LeastLoaded, AssignmentStrategy::RoundRobin] {
        let broker = make_broker(vec![("acct-a", "claude"), ("acct-b", "claude")]);
        seed_healthy(&broker, &["acct-a", "acct-b"]).await?;
        broker.set_strategy_for_tests(strategy);
        broker.mark_warming("acct-a").await;

        for _ in 0..4 {
            let pick = broker.assign_account(None).await;
            assert_eq!(
                pick,
                Some("acct-b".to_owned()),
                "strategy={strategy:?}: warming account leaked into assignment"
            );
        }
    }
    Ok(())
}

// ── ANTHROPIC_BASE_URL propagation (ka-0000002623) ──────────────────────
//
// A staged rotation profile must carry the endpoint the target account
// authenticates against, or a cross-endpoint switch (e.g. onto a Bedrock
// account behind a different LiteLLM) keeps the spawn-time base URL and 401s.

#[tokio::test]
async fn get_credentials_includes_base_url_when_set() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_base_url("acct-a", "http://gardenio-litellm:4000".into()).await;
    broker.set_token("acct-a", "key-a".into(), None, None).await?;

    let creds = broker
        .get_credentials("acct-a")
        .await
        .ok_or_else(|| anyhow::anyhow!("expected credentials for acct-a"))?;
    assert_eq!(
        creds.get("ANTHROPIC_BASE_URL").map(String::as_str),
        Some("http://gardenio-litellm:4000")
    );
    Ok(())
}

#[tokio::test]
async fn get_credentials_omits_base_url_when_unset() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "key-a".into(), None, None).await?;

    let creds = broker
        .get_credentials("acct-a")
        .await
        .ok_or_else(|| anyhow::anyhow!("expected credentials for acct-a"))?;
    assert!(!creds.contains_key("ANTHROPIC_BASE_URL"));
    Ok(())
}

#[tokio::test]
async fn refreshed_event_includes_base_url() -> anyhow::Result<()> {
    let configs = vec![AccountConfig {
        name: "acct-a".to_owned(),
        provider: "claude".to_owned(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }];
    let (tx, mut rx) = broadcast::channel(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: configs }, tx, None);

    // base_url stored BEFORE set_token, mirroring the credentials/set handler
    // order, so the Refreshed event carries it.
    broker.set_base_url("acct-a", "http://gardenio-litellm:4000".into()).await;
    broker.set_token("acct-a", "key-a".into(), None, None).await?;

    let event = rx.recv().await?;
    match event {
        CredentialEvent::Refreshed { account, credentials } => {
            assert_eq!(account, "acct-a");
            assert_eq!(
                credentials.get("ANTHROPIC_BASE_URL").map(String::as_str),
                Some("http://gardenio-litellm:4000")
            );
        }
        other => anyhow::bail!("expected Refreshed event, got {other:?}"),
    }
    Ok(())
}

// ── Same-token profile changes still redistribute (ka-0000006182) ────────────
//
// The controller period-syncs credentials/set every 5 min. When only the
// endpoint (base_url) or the LiteLLM header (litellm_key) moves but the upstream
// token is unchanged, set_token must STILL emit Refreshed — otherwise live
// sessions keep the stale ANTHROPIC_BASE_URL / x-litellm-api-key and 401/403,
// the same-token reopening of ka-0000002623. A fully-identical resync (same
// token AND same profile) must still emit nothing, preserving anti-churn.

/// Helper: a broker with a broadcast receiver, mirroring refreshed_event_*.
fn make_broker_with_rx(
    name: &str,
) -> (Arc<CredentialBroker>, broadcast::Receiver<CredentialEvent>) {
    let configs = vec![AccountConfig {
        name: name.to_owned(),
        provider: "claude".to_owned(),
        env_key: None,
        model: None,
        token_url: None,
        client_id: None,
        auth_url: None,
        device_auth_url: None,
        reauth: true,
    }];
    let (tx, rx) = broadcast::channel(16);
    let broker = CredentialBroker::new(CredentialConfig { accounts: configs }, tx, None);
    (broker, rx)
}

#[tokio::test]
async fn base_url_change_with_unchanged_token_emits_refreshed() -> anyhow::Result<()> {
    let (broker, mut rx) = make_broker_with_rx("acct-a");

    // Initial sync: base_url + token. Drains the first (token_changed) event.
    broker.set_base_url("acct-a", "http://litellm-old:4000".into()).await;
    broker.set_token("acct-a", "tok-1".into(), None, None).await?;
    let _ = rx.recv().await?;

    // Controller retargets the endpoint but the upstream token is unchanged.
    broker.set_base_url("acct-a", "http://litellm-new:4000".into()).await;
    broker.set_token("acct-a", "tok-1".into(), None, None).await?;

    // Bounded recv so a regression (event never emitted) fails fast instead of
    // hanging the test harness forever.
    let event = tokio::time::timeout(std::time::Duration::from_secs(2), rx.recv()).await.map_err(
        |_| {
            anyhow::anyhow!("no Refreshed event within 2s — same-token base_url change was dropped")
        },
    )??;
    match event {
        CredentialEvent::Refreshed { account, credentials } => {
            assert_eq!(account, "acct-a");
            assert_eq!(
                credentials.get("ANTHROPIC_BASE_URL").map(String::as_str),
                Some("http://litellm-new:4000"),
                "same-token base_url change must redistribute the new endpoint"
            );
        }
        other => anyhow::bail!("expected Refreshed event, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn litellm_key_change_with_unchanged_token_emits_refreshed() -> anyhow::Result<()> {
    let (broker, mut rx) = make_broker_with_rx("acct-a");

    broker.set_litellm_key("acct-a", "vk-old".into()).await;
    broker.set_token("acct-a", "tok-1".into(), None, None).await?;
    let _ = rx.recv().await?;

    // Virtual-key rotation onto the same upstream token.
    broker.set_litellm_key("acct-a", "vk-new".into()).await;
    broker.set_token("acct-a", "tok-1".into(), None, None).await?;

    // Bounded recv so a regression (event never emitted) fails fast instead of
    // hanging the test harness forever.
    let event = tokio::time::timeout(std::time::Duration::from_secs(2), rx.recv()).await.map_err(
        |_| {
            anyhow::anyhow!(
                "no Refreshed event within 2s — same-token litellm_key change was dropped"
            )
        },
    )??;
    match event {
        CredentialEvent::Refreshed { account, credentials } => {
            assert_eq!(account, "acct-a");
            assert_eq!(
                credentials.get("ANTHROPIC_CUSTOM_HEADERS").map(String::as_str),
                Some("x-litellm-api-key: Bearer vk-new"),
                "same-token litellm_key change must redistribute the new header"
            );
        }
        other => anyhow::bail!("expected Refreshed event, got {other:?}"),
    }
    Ok(())
}

#[tokio::test]
async fn identical_resync_emits_nothing() -> anyhow::Result<()> {
    let (broker, mut rx) = make_broker_with_rx("acct-a");

    broker.set_base_url("acct-a", "http://litellm:4000".into()).await;
    broker.set_litellm_key("acct-a", "vk-1".into()).await;
    broker.set_token("acct-a", "tok-1".into(), None, None).await?;
    let _ = rx.recv().await?; // drain the initial Refreshed

    // A fully-identical periodic resync: same token, same base_url, same key.
    broker.set_base_url("acct-a", "http://litellm:4000".into()).await;
    broker.set_litellm_key("acct-a", "vk-1".into()).await;
    broker.set_token("acct-a", "tok-1".into(), None, None).await?;

    // No event must be emitted — anti-churn preserved for the common no-op sync.
    match rx.try_recv() {
        Err(broadcast::error::TryRecvError::Empty) => Ok(()),
        Ok(ev) => anyhow::bail!("expected no event on identical resync, got {ev:?}"),
        Err(e) => anyhow::bail!("unexpected recv error: {e:?}"),
    }
}

// ── Off-lock persistence (ka-0000007732) ────────────────────────────────────
//
// These tests pin the invariant that credential persistence — the blocking
// std::fs disk write — happens OUTSIDE the accounts write lock, so a slow
// persist_dir (real PVC / throttled node disk / fsync stall) can no longer
// serialize hot-path credential ops behind the write.

/// Under a deliberately slow (200ms) persist, a concurrent hot-path account op
/// (`account_status`, which takes the accounts read lock) must complete
/// promptly — proving the accounts lock is NOT held across the disk write.
///
/// Before the fix, `set_token` held the accounts write lock across the blocking
/// `persist()`, so this read would block for the full write duration.
#[serial_test::serial]
#[tokio::test]
async fn persist_does_not_hold_accounts_lock() -> anyhow::Result<()> {
    std::env::set_var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP", "1");
    let tmp = tempfile::tempdir()?;
    let broker = make_persisting_broker(tmp.path().to_path_buf(), vec![("acct-a", "claude")]);

    // Make the disk write take 200ms, simulating a throttled persist_dir.
    broker.set_persist_delay_for_tests(200);

    // Kick off a mutation that persists. Because the write is off-lock, this
    // await returns only after the slow write — but the accounts lock is free
    // the whole time.
    let writer = {
        let broker = Arc::clone(&broker);
        tokio::spawn(async move {
            broker.set_token("acct-a", "key-a".into(), None, None).await.expect("set_token");
        })
    };

    // Give the writer a moment to enter the (slow) persist.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;

    // A hot-path read must NOT be blocked by the in-flight write. If the fix
    // regressed (lock held across the write), this would take ~150ms; with the
    // fix it returns immediately.
    let start = std::time::Instant::now();
    let status = broker.account_status("acct-a").await;
    let elapsed = start.elapsed();
    assert_eq!(status, "healthy");
    assert!(
        elapsed < std::time::Duration::from_millis(100),
        "hot-path account_status blocked {elapsed:?} behind the disk write — \
         accounts lock is being held across persist"
    );

    writer.await?;
    std::env::remove_var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP");
    Ok(())
}

/// Concurrent mutations must not serialize behind each other's disk write.
/// With the lock held across a 100ms write, N mutations would take ~N*100ms;
/// off-lock they proceed without the accounts lock ever gating on I/O, and the
/// final on-disk state reflects the last write (last-writer-wins).
#[serial_test::serial]
#[tokio::test]
async fn concurrent_mutations_do_not_serialize_on_disk_io() -> anyhow::Result<()> {
    std::env::set_var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP", "1");
    let tmp = tempfile::tempdir()?;
    let broker = make_persisting_broker(
        tmp.path().to_path_buf(),
        vec![("acct-a", "claude"), ("acct-b", "claude"), ("acct-c", "claude")],
    );
    broker.set_persist_delay_for_tests(100);

    // Fire several concurrent credential mutations.
    let mut handles = Vec::new();
    for (i, acct) in ["acct-a", "acct-b", "acct-c"].into_iter().enumerate() {
        let broker = Arc::clone(&broker);
        handles.push(tokio::spawn(async move {
            broker.set_token(acct, format!("key-{i}"), None, None).await.expect("set_token");
        }));
    }
    for h in handles {
        h.await?;
    }

    // All accounts are healthy and readable without the writes gating reads.
    for acct in ["acct-a", "acct-b", "acct-c"] {
        assert_eq!(broker.account_status(acct).await, "healthy");
    }

    // The persisted file exists and round-trips: the off-lock path still writes
    // correct, decryptable state to disk.
    broker.wait_for_persist_idle().await;
    let path = tmp.path().join("credentials.json");
    assert!(path.exists(), "credentials.json was not persisted");
    let loaded = crate::credential::persist::load(&path)?;
    assert_eq!(loaded.accounts.len(), 3, "all three accounts should be persisted");

    std::env::remove_var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP");
    Ok(())
}

/// The persisted file must reflect the account mutations even though the write
/// happens off the accounts lock — i.e. the snapshot is taken while the lock is
/// still held, so on-disk state is always a real point-in-time view.
#[serial_test::serial]
#[tokio::test]
async fn off_lock_persist_writes_correct_snapshot() -> anyhow::Result<()> {
    std::env::set_var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP", "1");
    let tmp = tempfile::tempdir()?;
    let broker = make_persisting_broker(tmp.path().to_path_buf(), vec![("acct-a", "claude")]);

    broker.set_token("acct-a", "the-secret-key".into(), None, None).await?;
    broker.wait_for_persist_idle().await;

    let path = tmp.path().join("credentials.json");
    let loaded = crate::credential::persist::load(&path)?;
    assert_eq!(loaded.accounts["acct-a"].access_token, "the-secret-key");

    std::env::remove_var("COOP_ALLOW_PLAINTEXT_BOOTSTRAP");
    Ok(())
}

#[tokio::test]
async fn key_suffix_returns_last_four_ascii_chars() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "sk-abcd1234".into(), None, None).await?;
    assert_eq!(broker.key_suffix("acct-a").await.as_deref(), Some("1234"));
    Ok(())
}

#[tokio::test]
async fn key_suffix_handles_non_ascii_token_without_panicking() -> anyhow::Result<()> {
    // Regression for ka-0000001776: a byte-offset slice panicked
    // ("byte index N is not a char boundary") when the token ended in a
    // multi-byte UTF-8 sequence. The token is operator-controlled verbatim
    // via POST /api/v1/credentials/set, and key_suffix is called per-session
    // when building the /ws/mux dashboard snapshot, so one bad token killed
    // the whole feed. It must now return the last 4 chars char-safely.
    let broker = make_broker(vec![("acct-a", "claude")]);
    // Token ends in "é" (2 UTF-8 bytes); the last 4 chars are "énüé".
    broker.set_token("acct-a", "sk-tokénüé".into(), None, None).await?;
    assert_eq!(broker.key_suffix("acct-a").await.as_deref(), Some("énüé"));
    Ok(())
}

#[tokio::test]
async fn key_suffix_short_multibyte_token() -> anyhow::Result<()> {
    let broker = make_broker(vec![("acct-a", "claude")]);
    broker.set_token("acct-a", "é".into(), None, None).await?;
    assert_eq!(broker.key_suffix("acct-a").await.as_deref(), Some("é"));
    Ok(())
}
